// Copyright 2022 Jetpack Technologies Inc and contributors. All rights reserved.
// Use of this source code is governed by the license in the LICENSE file.

// Package devbox creates isolated development environments.
package impl

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/fatih/color"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.jetpack.io/devbox/internal/boxcli/featureflag"
	"go.jetpack.io/devbox/internal/boxcli/generate"
	"go.jetpack.io/devbox/internal/boxcli/usererr"
	"go.jetpack.io/devbox/internal/cuecfg"
	"go.jetpack.io/devbox/internal/debug"
	"go.jetpack.io/devbox/internal/fileutil"
	"go.jetpack.io/devbox/internal/initrec"
	"go.jetpack.io/devbox/internal/nix"
	"go.jetpack.io/devbox/internal/planner"
	"go.jetpack.io/devbox/internal/planner/plansdk"
	"go.jetpack.io/devbox/internal/plugin"
	"go.jetpack.io/devbox/internal/services"
	"go.jetpack.io/devbox/internal/telemetry"
	"go.jetpack.io/devbox/internal/ux"
	"golang.org/x/exp/slices"
)

const (
	// configFilename is name of the JSON file that defines a devbox environment.
	configFilename = "devbox.json"

	// shellHistoryFile keeps the history of commands invoked inside devbox shell
	shellHistoryFile = ".devbox/shell_history"

	scriptsDir           = ".devbox/gen/scripts"
	hooksFilename        = ".hooks"
	arbitraryCmdFilename = ".cmd"
)

func InitConfig(dir string, writer io.Writer) (created bool, err error) {
	cfgPath := filepath.Join(dir, configFilename)

	config := &Config{
		Nixpkgs: NixpkgsConfig{
			Commit: plansdk.DefaultNixpkgsCommit,
		},
	}
	if featureflag.EnvConfig.Enabled() {
		// TODO: after removing feature flag we can decide if we want
		// to have omitempty for Env in Config or not.
		config.Env = map[string]string{}
	}
	// package suggestion
	pkgsToSuggest, err := initrec.Get(dir)
	if err != nil {
		return false, err
	}
	if len(pkgsToSuggest) > 0 {
		s := fmt.Sprintf("devbox add %s", strings.Join(pkgsToSuggest, " "))
		fmt.Fprintf(
			writer,
			"We detected extra packages you may need. To install them, run `%s`\n",
			color.HiYellowString(s),
		)
	}

	return cuecfg.InitFile(cfgPath, config)
}

type Devbox struct {
	cfg *Config
	// projectDir is the directory where the config file (devbox.json) resides
	projectDir    string
	pluginManager *plugin.Manager
	writer        io.Writer
}

func Open(path string, writer io.Writer) (*Devbox, error) {

	projectDir, err := findProjectDir(path)
	if err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(projectDir, configFilename)

	cfg, err := ReadConfig(cfgPath)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if err = upgradeConfig(cfg, cfgPath); err != nil {
		return nil, err
	}

	box := &Devbox{
		cfg:           cfg,
		projectDir:    projectDir,
		pluginManager: plugin.NewManager(),
		writer:        writer,
	}
	return box, nil
}

func (d *Devbox) ProjectDir() string {
	return d.projectDir
}

func (d *Devbox) Config() *Config {
	return d.cfg
}

// TODO savil. move to packages.go
func (d *Devbox) Add(pkgs ...string) error {
	original := d.cfg.RawPackages
	// Check packages are valid before adding.
	for _, pkg := range pkgs {
		ok := nix.PkgExists(d.cfg.Nixpkgs.Commit, pkg)
		if !ok {
			return errors.WithMessage(nix.ErrPackageNotFound, pkg)
		}
	}

	// Add to Packages to config only if it's not already there
	for _, pkg := range pkgs {
		if slices.Contains(d.cfg.RawPackages, pkg) {
			continue
		}
		d.cfg.RawPackages = append(d.cfg.RawPackages, pkg)
	}
	if err := d.saveCfg(); err != nil {
		return err
	}

	d.pluginManager.ApplyOptions(plugin.WithAddMode())
	if err := d.ensurePackagesAreInstalled(install); err != nil {
		// if error installing, revert devbox.json
		// This is not perfect because there may be more than 1 package being
		// installed and we don't know which one failed. But it's better than
		// blindly add all packages.
		color.New(color.FgRed).Fprintf(
			d.writer,
			"There was an error installing nix packages: %v. "+
				"Packages were not added to devbox.json\n",
			strings.Join(pkgs, ", "),
		)
		d.cfg.RawPackages = original
		_ = d.saveCfg() // ignore error to ensure we return the original error
		return err
	}

	for _, pkg := range pkgs {
		if err := plugin.PrintReadme(
			pkg,
			d.projectDir,
			d.writer,
			false, /*markdown*/
		); err != nil {
			return err
		}
	}

	return d.printPackageUpdateMessage(install, pkgs)
}

// TODO savil. move to packages.go
func (d *Devbox) Remove(pkgs ...string) error {

	// First, save which packages are being uninstalled. Do this before we modify d.cfg.RawPackages below.
	uninstalledPackages := lo.Intersect(d.cfg.RawPackages, pkgs)

	var missingPkgs []string
	d.cfg.RawPackages, missingPkgs = lo.Difference(d.cfg.RawPackages, pkgs)

	if len(missingPkgs) > 0 {
		ux.Fwarning(
			d.writer,
			"the following packages were not found in your devbox.json: %s\n",
			strings.Join(missingPkgs, ", "),
		)
	}
	if err := d.saveCfg(); err != nil {
		return err
	}

	if err := plugin.Remove(d.projectDir, uninstalledPackages); err != nil {
		return err
	}

	if err := d.removePackagesFromProfile(uninstalledPackages); err != nil {
		return err
	}

	if err := d.ensurePackagesAreInstalled(uninstall); err != nil {
		return err
	}

	return d.printPackageUpdateMessage(uninstall, uninstalledPackages)
}

func (d *Devbox) ShellPlan() (*plansdk.ShellPlan, error) {
	userDefinedPkgs := d.packages()
	shellPlan := planner.GetShellPlan(d.projectDir, userDefinedPkgs)
	shellPlan.DevPackages = userDefinedPkgs

	nixpkgsInfo, err := plansdk.GetNixpkgsInfo(d.cfg.Nixpkgs.Commit)
	if err != nil {
		return nil, err
	}
	shellPlan.NixpkgsInfo = nixpkgsInfo

	return shellPlan, nil
}

func (d *Devbox) Generate() error {
	if err := d.generateShellFiles(); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (d *Devbox) Shell() error {
	if err := d.ensurePackagesAreInstalled(ensure); err != nil {
		return err
	}
	fmt.Fprintln(d.writer, "Starting a devbox shell...")

	profileDir, err := d.profilePath()
	if err != nil {
		return err
	}

	pluginHooks, err := plugin.InitHooks(d.packages(), d.projectDir)
	if err != nil {
		return err
	}

	var env map[string]string
	if featureflag.UnifiedEnv.Enabled() {
		env, err = d.computeNixEnv()
		if err != nil {
			return err
		}
	} else {
		env, err = plugin.Env(d.packages(), d.projectDir)
		if err != nil {
			return err
		}
	}

	shellStartTime := os.Getenv("DEVBOX_SHELL_START_TIME")
	if shellStartTime == "" {
		shellStartTime = telemetry.UnixTimestampFromTime(telemetry.CommandStartTime())
	}

	opts := []nix.ShellOption{
		nix.WithPluginInitHook(strings.Join(pluginHooks, "\n")),
		nix.WithProfile(profileDir),
		nix.WithHistoryFile(filepath.Join(d.projectDir, shellHistoryFile)),
		nix.WithProjectDir(d.projectDir),
		nix.WithEnvVariables(env),
		nix.WithPKGConfigDir(d.pluginVirtenvPath()),
		nix.WithShellStartTime(shellStartTime),
	}

	shell, err := nix.NewDevboxShell(d.cfg.Nixpkgs.Commit, opts...)
	if err != nil {
		return err
	}

	shell.UserInitHook = d.cfg.Shell.InitHook.String()
	return shell.Run(d.nixShellFilePath(), d.nixFlakesFilePath())
}

func (d *Devbox) RunScript(cmdName string, cmdArgs []string) error {
	if featureflag.UnifiedEnv.Disabled() {
		return d.RunScriptInNewNixShell(cmdName)
	}

	if err := d.ensurePackagesAreInstalled(ensure); err != nil {
		return err
	}

	if err := d.writeScriptsToFiles(); err != nil {
		return err
	}

	env, err := d.computeNixEnv()
	if err != nil {
		return err
	}

	var cmdWithArgs []string
	if _, ok := d.cfg.Shell.Scripts[cmdName]; ok {
		// it's a script, so replace the command with the script file's path.
		cmdWithArgs = append([]string{d.scriptPath(d.scriptFilename(cmdName))}, cmdArgs...)
	} else {
		// Arbitrary commands should also run the hooks, so we write them to a file as well. However, if the
		// command args include env variable evaluations, then they'll be evaluated _before_ the hooks run,
		// which we don't want. So, one solution is to write the entire command and its arguments into the
		// file itself, but that may not be great if the variables contain sensitive information. Instead,
		// we save the entire command (with args) into the DEVBOX_RUN_CMD var, and then the script evals it.
		err := d.writeScriptFile(arbitraryCmdFilename, d.scriptBody("eval $DEVBOX_RUN_CMD\n"))
		if err != nil {
			return err
		}
		cmdWithArgs = []string{d.scriptPath(d.scriptFilename(arbitraryCmdFilename))}
		env["DEVBOX_RUN_CMD"] = strings.Join(append([]string{cmdName}, cmdArgs...), " ")
	}

	return nix.RunScript(d.projectDir, strings.Join(cmdWithArgs, " "), env)
}

// RunScriptInNewNixShell implements `devbox run` (from outside a devbox shell) using a nix shell.
// Deprecated: RunScript should be used instead.
func (d *Devbox) RunScriptInNewNixShell(scriptName string) error {
	if err := d.ensurePackagesAreInstalled(ensure); err != nil {
		return err
	}
	fmt.Fprintln(d.writer, "Starting a devbox shell...")

	profileDir, err := d.profilePath()
	if err != nil {
		return err
	}

	script := d.cfg.Shell.Scripts[scriptName]
	if script == nil {
		return usererr.New("unable to find a script with name %s", scriptName)
	}

	pluginHooks, err := plugin.InitHooks(d.packages(), d.projectDir)
	if err != nil {
		return err
	}

	env, err := plugin.Env(d.packages(), d.projectDir)
	if err != nil {
		return err
	}

	opts := []nix.ShellOption{
		nix.WithPluginInitHook(strings.Join(pluginHooks, "\n")),
		nix.WithProfile(profileDir),
		nix.WithHistoryFile(filepath.Join(d.projectDir, shellHistoryFile)),
		nix.WithUserScript(scriptName, script.String()),
		nix.WithProjectDir(d.projectDir),
		nix.WithEnvVariables(env),
		nix.WithPKGConfigDir(d.pluginVirtenvPath()),
	}

	shell, err := nix.NewDevboxShell(d.cfg.Nixpkgs.Commit, opts...)

	if err != nil {
		fmt.Fprint(d.writer, err)
		return err
	}

	shell.UserInitHook = d.cfg.Shell.InitHook.String()
	return shell.Run(d.nixShellFilePath(), d.nixFlakesFilePath())
}

// TODO: deprecate in favor of RunScript().
func (d *Devbox) RunScriptInShell(scriptName string) error {
	profileDir, err := d.profilePath()
	if err != nil {
		return err
	}

	script := d.cfg.Shell.Scripts[scriptName]
	if script == nil {
		return usererr.New("unable to find a script with name %s", scriptName)
	}

	shell, err := nix.NewDevboxShell(
		d.cfg.Nixpkgs.Commit,
		nix.WithProfile(profileDir),
		nix.WithHistoryFile(filepath.Join(d.projectDir, shellHistoryFile)),
		nix.WithUserScript(scriptName, script.String()),
		nix.WithProjectDir(d.projectDir),
	)

	if err != nil {
		fmt.Fprint(d.writer, err)
		return err
	}

	return shell.RunInShell()
}

func (d *Devbox) ListScripts() []string {
	keys := make([]string, len(d.cfg.Shell.Scripts))
	i := 0
	for k := range d.cfg.Shell.Scripts {
		keys[i] = k
		i++
	}
	return keys
}

// TODO: deprecate in favor of RunScript().
func (d *Devbox) ExecWithShell(cmds ...string) error {
	if err := d.ensurePackagesAreInstalled(ensure); err != nil {
		return err
	}

	profileBinPath, err := d.profileBinPath()
	if err != nil {
		return err
	}

	env, err := plugin.Env(d.packages(), d.projectDir)
	if err != nil {
		return err
	}

	virtenvBinPath := filepath.Join(d.projectDir, plugin.VirtenvBinPath) + ":"

	pathWithProfileBin := fmt.Sprintf("PATH=%s%s:$PATH", virtenvBinPath, profileBinPath)
	cmds = append([]string{pathWithProfileBin}, cmds...)

	return nix.Exec(d.nixShellFilePath(), cmds, env)
}

// TODO: deprecate in favor of RunScript().
func (d *Devbox) Exec(cmds ...string) error {
	if featureflag.UnifiedEnv.Disabled() {
		return d.ExecWithShell(cmds...)
	}
	if len(cmds) > 0 {
		return d.RunScript(cmds[0], cmds[1:])
	}
	return errors.Errorf("cannot execute empty command: %v", cmds)
}

func (d *Devbox) PrintEnv() (string, error) {
	script := ""
	if featureflag.UnifiedEnv.Disabled() {
		envs, err := plugin.Env(d.packages(), d.projectDir)
		if err != nil {
			return "", err
		}
		for k, v := range envs {
			script += fmt.Sprintf("export %s=%s\n", k, v)
		}
		return script, nil
	}
	envs, err := d.computeNixEnv()
	if err != nil {
		return "", err
	}

	for k, v := range envs {
		// %q is for escaping quotes in env variables that
		// have quotes in them e.g., shellHook
		script += fmt.Sprintf("export %s=%q\n", k, v)
	}

	return script, nil
}

func (d *Devbox) Info(pkg string, markdown bool) error {
	info, hasInfo := nix.PkgInfo(d.cfg.Nixpkgs.Commit, pkg)
	if !hasInfo {
		_, err := fmt.Fprintf(d.writer, "Package %s not found\n", pkg)
		return errors.WithStack(err)
	}
	if _, err := fmt.Fprintf(
		d.writer,
		"%s%s\n",
		lo.Ternary(markdown, "## ", ""),
		info,
	); err != nil {
		return errors.WithStack(err)
	}
	return plugin.PrintReadme(
		pkg,
		d.projectDir,
		d.writer,
		markdown,
	)
}

// generates devcontainer.json and Dockerfile for vscode run-in-container
// and Github Codespaces
func (d *Devbox) GenerateDevcontainer(force bool) error {
	// construct path to devcontainer directory
	devContainerPath := filepath.Join(d.projectDir, ".devcontainer/")
	devContainerJSONPath := filepath.Join(devContainerPath, "devcontainer.json")
	dockerfilePath := filepath.Join(devContainerPath, "Dockerfile")

	// check if devcontainer.json or Dockerfile exist
	filesExist := plansdk.FileExists(devContainerJSONPath) || plansdk.FileExists(dockerfilePath)

	if force || !filesExist {
		// create directory
		err := os.MkdirAll(devContainerPath, os.ModePerm)
		if err != nil {
			return errors.WithStack(err)
		}
		// generate dockerfile
		err = generate.CreateDockerfile(tmplFS, devContainerPath)
		if err != nil {
			return errors.WithStack(err)
		}
		// generate devcontainer.json
		err = generate.CreateDevcontainer(devContainerPath, d.packages())
		if err != nil {
			return errors.WithStack(err)
		}
	} else {
		return usererr.New(
			"Files devcontainer.json or Dockerfile are already present in .devcontainer/. " +
				"Remove the files or use --force to overwrite them.",
		)
	}
	return nil
}

// generates a Dockerfile that replicates the devbox shell
func (d *Devbox) GenerateDockerfile(force bool) error {
	dockerfilePath := filepath.Join(d.projectDir, "Dockerfile")
	// check if Dockerfile doesn't exist
	filesExist := plansdk.FileExists(dockerfilePath)
	if force || !filesExist {
		// generate dockerfile
		err := generate.CreateDockerfile(tmplFS, d.projectDir)
		if err != nil {
			return errors.WithStack(err)
		}
	} else {
		return usererr.New(
			"Dockerfile is already present in the current directory. " +
				"Remove it or use --force to overwrite it.",
		)
	}

	return nil
}

// generates a .envrc file that makes direnv integration convenient
func (d *Devbox) GenerateEnvrc(force bool, source string) error {
	envrcfilePath := filepath.Join(d.projectDir, ".envrc")
	filesExist := fileutil.Exists(envrcfilePath)
	// confirm .envrc doesn't exist and don't overwrite an existing .envrc
	if force || !filesExist {
		if commandExists("direnv") {
			// prompt for direnv allow
			var result string
			prompt := &survey.Input{
				Message: "Do you want to enable direnv integration for this devbox project? [y/N]",
			}
			err := survey.AskOne(prompt, &result)
			if err != nil {
				return errors.WithStack(err)
			}

			if strings.ToLower(result) == "y" {
				// .envrc file creation
				err := generate.CreateEnvrc(tmplFS, d.projectDir)
				if err != nil {
					return errors.WithStack(err)
				}
				cmd := exec.Command("direnv", "allow")
				err = cmd.Run()
				if err != nil {
					return errors.WithStack(err)
				}
			} else if source == "generate" {
				// .envrc file creation
				err := generate.CreateEnvrc(tmplFS, d.projectDir)
				if err != nil {
					return errors.WithStack(err)
				}
			}
		}
	} else {
		return usererr.New(
			"A .envrc is already present in the current directory. " +
				"Remove it or use --force to overwrite it.",
		)
	}

	return nil
}

// saveCfg writes the config file to the devbox directory.
func (d *Devbox) saveCfg() error {
	cfgPath := filepath.Join(d.projectDir, configFilename)
	return cuecfg.WriteFile(cfgPath, d.cfg)
}

func (d *Devbox) Services() (plugin.Services, error) {
	return plugin.GetServices(d.packages(), d.projectDir)
}

func (d *Devbox) StartServices(ctx context.Context, serviceNames ...string) error {
	if !IsDevboxShellEnabled() {
		return d.Exec(append([]string{"devbox", "services", "start"}, serviceNames...)...)
	}
	return services.Start(ctx, d.packages(), serviceNames, d.projectDir, d.writer)
}

func (d *Devbox) StartProcessManager(ctx context.Context) error {
	svcs, err := d.Services()
	if err != nil {
		return err
	}
	hasServiceWithProcessCompose := false
	for _, s := range svcs {
		if _, hasComposeYaml := s.ProcessComposeYaml(); hasComposeYaml {
			hasServiceWithProcessCompose = true
			break
		}
	}
	if !hasServiceWithProcessCompose {
		return usererr.New("No services with process-compose.yaml found")
	}
	processComposePath, err := utilityLookPath("process-compose")
	if err != nil {
		fmt.Fprintln(d.writer, "Installing process-compose. This may take a minute but will only happen once.")
		if err = d.addDevboxUtilityPackage("process-compose"); err != nil {
			return err
		}
	}
	if !IsDevboxShellEnabled() {
		return d.Exec("devbox", "services", "manager")
	}

	return services.StartProcessManager(ctx, processComposePath, svcs)
}

func (d *Devbox) StopServices(ctx context.Context, serviceNames ...string) error {
	if !IsDevboxShellEnabled() {
		return d.Exec(append([]string{"devbox", "services", "stop"}, serviceNames...)...)
	}
	return services.Stop(ctx, d.packages(), serviceNames, d.projectDir, d.writer)
}

func (d *Devbox) generateShellFiles() error {
	plan, err := d.ShellPlan()
	if err != nil {
		return err
	}
	return generateForShell(d.projectDir, plan, d.pluginManager)
}

// installMode is an enum for helping with ensurePackagesAreInstalled implementation
type installMode string

const (
	install   installMode = "install"
	uninstall installMode = "uninstall"
	ensure    installMode = "ensure"
)

// TODO savil. move to packages.go
func (d *Devbox) ensurePackagesAreInstalled(mode installMode) error {
	if err := d.generateShellFiles(); err != nil {
		return err
	}
	if mode == ensure {
		fmt.Fprintln(d.writer, "Ensuring packages are installed.")
	}

	if featureflag.Flakes.Enabled() {
		if err := d.addPackagesToProfile(mode); err != nil {
			return err
		}

	} else {
		if mode == install || mode == uninstall {
			installingVerb := "Installing"
			if mode == uninstall {
				installingVerb = "Uninstalling"
			}
			_, _ = fmt.Fprintf(d.writer, "%s nix packages.\n", installingVerb)
		}

		// We need to re-install the packages
		if err := d.installNixProfile(); err != nil {
			fmt.Fprintln(d.writer)
			return errors.Wrap(err, "apply Nix derivation")
		}
	}

	return plugin.RemoveInvalidSymlinks(d.projectDir)
}

// TODO savil. move to packages.go
func (d *Devbox) printPackageUpdateMessage(
	mode installMode,
	pkgs []string,
) error {
	verb := "installed"
	var infos []*nix.Info
	for _, pkg := range pkgs {
		info, _ := nix.PkgInfo(d.cfg.Nixpkgs.Commit, pkg)
		infos = append(infos, info)
	}
	if mode == uninstall {
		verb = "removed"
	}

	if len(pkgs) > 0 {

		successMsg := fmt.Sprintf("%s (%s) is now %s.\n", pkgs[0], infos[0], verb)
		if len(pkgs) > 1 {
			pkgsWithVersion := []string{}
			for idx, pkg := range pkgs {
				pkgsWithVersion = append(
					pkgsWithVersion,
					fmt.Sprintf("%s (%s)", pkg, infos[idx]),
				)
			}
			successMsg = fmt.Sprintf(
				"%s are now %s.\n",
				strings.Join(pkgsWithVersion, ", "),
				verb,
			)
		}
		fmt.Fprint(d.writer, successMsg)

		// (Only when in devbox shell) Prompt the user to run hash -r
		// to ensure we refresh the shell hash and load the proper environment.
		if IsDevboxShellEnabled() {
			if err := plugin.PrintEnvUpdateMessage(
				lo.Ternary(mode == install, pkgs, []string{}),
				d.projectDir,
				d.writer,
			); err != nil {
				return err
			}
		}
	} else {
		fmt.Fprintf(d.writer, "No packages %s.\n", verb)
	}
	return nil
}

// computeNixEnv computes the set of environment variables that define a Devbox
// environment. The "devbox run" and "devbox shell" commands source these
// variables into a shell before executing a command or showing an interactive
// prompt.
//
// The process for building the environment involves layering sets of
// environment variables on top of each other, with each layer overwriting any
// duplicate keys from the previous:
//
//  1. Copy variables from the current environment except for those in
//     ignoreCurrentEnvVar, such as PWD and SHELL.
//  2. Copy variables from "nix print-dev-env" except for those in
//     ignoreDevEnvVar, such as TMPDIR and HOME.
//  3. Copy variables from Devbox plugins.
//  4. Set PATH to the concatenation of the PATHs from step 3, step 2, and
//     step 1 (in that order).
//
// The final result is a set of environment variables where Devbox plugins have
// the highest priority, then Nix environment variables, and then variables
// from the current environment. Similarly, the PATH gives Devbox plugin
// binaries the highest priority, then Nix packages, and then non-Nix
// programs.
//
// Note that the shellrc.tmpl template (which sources this environment) does
// some additional processing. The computeNixEnv environment won't necessarily
// represent the final "devbox run" or "devbox shell" environments.
func (d *Devbox) computeNixEnv() (map[string]string, error) {
	currentEnv := os.Environ()
	env := make(map[string]string, len(currentEnv))
	for _, kv := range currentEnv {
		key, val, found := strings.Cut(kv, "=")
		if !found {
			return nil, errors.Errorf("expected \"=\" in keyval: %s", kv)
		}
		if ignoreCurrentEnvVar[key] {
			continue
		}
		env[key] = val
	}
	currentEnvPath := env["PATH"]
	debug.Log("current environment PATH is: %s", currentEnvPath)

	vaf, err := nix.PrintDevEnv(d.nixShellFilePath(), d.nixFlakesFilePath())
	if err != nil {
		return nil, err
	}

	// Add environment variables from "nix print-dev-env" except for a few
	// special ones we need to ignore.
	for key, val := range vaf.Variables {
		// We only care about "exported" because the var and array types seem to only be used by nix-defined
		// functions that we don't need (like genericBuild). For reference, each type translates to bash as follows:
		// var: export VAR=VAL
		// exported: export VAR=VAL
		// array: declare -a VAR=('VAL1' 'VAL2' )
		if val.Type != "exported" {
			continue
		}

		// SSL_CERT_FILE is a special-case. We only ignore it if it's
		// set to a specific value. This emulates the behavior of
		// "nix develop".
		if key == "SSL_CERT_FILE" && val.Value.(string) == "/no-cert-file.crt" {
			continue
		}

		// Certain variables get set to invalid values after Nix builds
		// the shell environment. For example, HOME=/homeless-shelter
		// and TMPDIR points to a missing directory. We want to ignore
		// those values and just use the values from the current
		// environment instead.
		if ignoreDevEnvVar[key] {
			continue
		}

		env[key] = val.Value.(string)
	}
	nixEnvPath := env["PATH"]
	debug.Log("nix environment PATH is: %s", nixEnvPath)

	// These variables are only needed for shell, but we include them here in the computed env
	// for both shell and run in order to be as identical as possible.
	env["__ETC_PROFILE_NIX_SOURCED"] = "1" // Prevent user init file from loading nix profiles
	env["DEVBOX_SHELL_ENABLED"] = "1"      // Used to determine whether we're inside a shell (e.g. to prevent shell inception)

	// Add any vars defined in plugins.
	pluginEnv, err := plugin.Env(d.packages(), d.projectDir)
	if err != nil {
		return nil, err
	}
	for k, v := range pluginEnv {
		env[k] = v
	}

	// Include env variables in devbox.json
	if featureflag.EnvConfig.Enabled() {
		// TODO: if the uer defines PATH here, how should it be handled?
		for k, v := range d.configEnvs(env) {
			env[k] = v
		}
	}

	// TODO: consider removing this; not being used?
	pluginVirtenvPath := d.pluginVirtenvPath()
	debug.Log("plugin virtual environment PATH is: %s", pluginVirtenvPath)

	env["PATH"] = nix.JoinPathLists(pluginVirtenvPath, nixEnvPath, currentEnvPath)
	debug.Log("computed unified environment PATH is: %s", env["PATH"])

	return env, nil
}

// TODO savil. move to packages.go
// installNixProfile installs or uninstalls packages to or from this
// devbox's Nix profile so that it matches what's in development.nix
func (d *Devbox) installNixProfile() (err error) {
	profileDir, err := d.profilePath()
	if err != nil {
		return err
	}

	cmd := exec.Command(
		"nix-env",
		"--profile", profileDir,
		"--install",
		"-f", filepath.Join(d.projectDir, ".devbox/gen/development.nix"),
	)

	cmd.Env = nix.DefaultEnv()
	cmd.Stdout = &nix.PackageInstallWriter{Writer: d.writer}

	cmd.Stderr = cmd.Stdout

	err = cmd.Run()

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return errors.Errorf("running command %s: exit status %d with command stderr: %s",
			cmd, exitErr.ExitCode(), string(exitErr.Stderr))
	}
	if err != nil {
		return errors.Errorf("running command %s: %v", cmd, err)
	}
	return nil
}

// writeScriptsToFiles writes scripts defined in devbox.json into files inside .devbox/gen/scripts.
// Scripts (and hooks) are persisted so that we can easily call them from devbox run (inside or outside shell).
func (d *Devbox) writeScriptsToFiles() error {
	err := os.MkdirAll(filepath.Join(d.projectDir, scriptsDir), 0755) // Ensure directory exists.
	if err != nil {
		return errors.WithStack(err)
	}

	// Read dir contents before writing, so we can clean up later.
	entries, err := os.ReadDir(filepath.Join(d.projectDir, scriptsDir))
	if err != nil {
		return errors.WithStack(err)
	}

	// Write all hooks to a file.
	written := map[string]struct{}{} // set semantics; value is irrelevant
	pluginHooks, err := plugin.InitHooks(d.packages(), d.projectDir)
	if err != nil {
		return errors.WithStack(err)
	}
	hooks := strings.Join(append([]string{d.cfg.Shell.InitHook.String()}, pluginHooks...), "\n\n")
	// always write it, even if there are no hooks, because scripts will source it.
	err = d.writeScriptFile(hooksFilename, hooks)
	if err != nil {
		return errors.WithStack(err)
	}
	written[d.scriptFilename(hooksFilename)] = struct{}{}

	// Write scripts to files.
	for name, body := range d.cfg.Shell.Scripts {
		err = d.writeScriptFile(name, d.scriptBody(body.String()))
		if err != nil {
			return errors.WithStack(err)
		}
		written[d.scriptFilename(name)] = struct{}{}
	}

	// Delete any files that weren't written just now.
	for _, entry := range entries {
		if _, ok := written[entry.Name()]; !ok && !entry.IsDir() {
			err := os.Remove(d.scriptPath(entry.Name()))
			if err != nil {
				debug.Log("failed to clean up script file %s, error = %s", entry.Name(), err) // no need to fail run
			}
		}
	}

	return nil
}

func (d *Devbox) writeScriptFile(name string, body string) (err error) {
	script, err := os.Create(d.scriptPath(d.scriptFilename(name)))
	if err != nil {
		return errors.WithStack(err)
	}
	defer func() {
		cerr := script.Close()
		if err == nil {
			err = cerr
		}
	}()
	err = script.Chmod(0755)
	if err != nil {
		return errors.WithStack(err)
	}
	_, err = script.WriteString(body)
	return errors.WithStack(err)
}

func (d *Devbox) scriptPath(filename string) string {
	return filepath.Join(d.projectDir, scriptsDir, filename)
}

func (d *Devbox) scriptFilename(scriptName string) string {
	return scriptName + ".sh"
}

func (d *Devbox) scriptBody(body string) string {
	return fmt.Sprintf(". %s\n\n%s", d.scriptPath(d.scriptFilename(hooksFilename)), body)
}

func (d *Devbox) nixShellFilePath() string {
	return filepath.Join(d.projectDir, ".devbox/gen/shell.nix")
}

func (d *Devbox) nixFlakesFilePath() string {
	return filepath.Join(d.projectDir, ".devbox/gen/flake/flake.nix")
}

func (d *Devbox) packages() []string {
	return d.cfg.Packages(d.writer)
}

func (d *Devbox) pluginVirtenvPath() string {
	return filepath.Join(d.projectDir, plugin.VirtenvBinPath)
}

// configEnvs takes the computed env variables (nix + plugin) and adds env
// variables defined in Config. It also parses variables in config
// that are referenced by $VAR or ${VAR} and replaces them with
// their value in the computed env variables. Note, this doesn't
// allow env variables from outside the shell to be referenced so
// no leaked variables are caused by this function.
func (d *Devbox) configEnvs(computedEnv map[string]string) map[string]string {
	mapperfunc := func(value string) string {
		// Special variables that should return correct value
		switch value {
		case "PWD":
			return d.ProjectDir()
		}
		// check if referenced variables exists in computed environment
		if v, ok := computedEnv[value]; ok {
			return v
		}
		return ""
	}
	configEnvs := map[string]string{}
	// Include env variables in config
	for key, value := range d.cfg.Env {
		// parse values for "$VAR" or "${VAR}"
		parsedValue := os.Expand(value, mapperfunc)
		configEnvs[key] = parsedValue
	}
	return configEnvs
}

// Move to a utility package?
func IsDevboxShellEnabled() bool {
	inDevboxShell, err := strconv.ParseBool(os.Getenv("DEVBOX_SHELL_ENABLED"))
	if err != nil {
		return false
	}
	return inDevboxShell
}

func commandExists(command string) bool {
	_, err := exec.LookPath(command)
	return err == nil
}

// ignoreCurrentEnvVar contains environment variables that Devbox should remove
// from the slice of [os.Environ] variables before sourcing them. These are
// variables that are set automatically by a new shell.
var ignoreCurrentEnvVar = map[string]bool{
	// Devbox may change the working directory of the shell, so using the
	// original PWD and OLDPWD would be wrong.
	"PWD":    true,
	"OLDPWD": true,

	// SHLVL is the number of nested shells. Copying it would give the
	// Devbox shell the same level as the parent shell.
	"SHLVL": true,

	// The parent shell isn't guaranteed to be the same as the Devbox shell.
	"SHELL": true,
}

// ignoreDevEnvVar contains environment variables that Devbox should remove from
// the slice of [Devbox.PrintDevEnv] variables before sourcing them.
//
// This list comes directly from the "nix develop" source:
// https://github.com/NixOS/nix/blob/f08ad5bdbac02167f7d9f5e7f9bab57cf1c5f8c4/src/nix/develop.cc#L257-L275
var ignoreDevEnvVar = map[string]bool{
	"BASHOPTS":           true,
	"HOME":               true,
	"NIX_BUILD_TOP":      true,
	"NIX_ENFORCE_PURITY": true,
	"NIX_LOG_FD":         true,
	"NIX_REMOTE":         true,
	"PPID":               true,
	"SHELL":              true,
	"SHELLOPTS":          true,
	"TEMP":               true,
	"TEMPDIR":            true,
	"TERM":               true,
	"TMP":                true,
	"TMPDIR":             true,
	"TZ":                 true,
	"UID":                true,
}
