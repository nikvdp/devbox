---
title: Ruby
---

[**Example Repo**](https://github.com/jetpack-io/devbox-examples/tree/main/development/ruby)

[![Open In Devbox.sh](https://jetpack.io/img/devbox/open-in-devbox.svg)](https://devbox.sh/github.com/jetpack-io/devbox-examples?folder=development/ruby)

Ruby can be automatically configured by Devbox via the built-in Ruby Plugin. This plugin will activate automatically when you install Ruby 2.7 using `devbox add ruby`. 

## Adding Ruby to your shell

Run `devbox add ruby bundler`, or add the following to your `devbox.json`

```json
    "packages": [
        "ruby_3_1",
        "bundler"
    ]
```

This will install Ruby 3.1 to your shell. 

Other versions available include:  

* `ruby` (Ruby 2.7)
* `ruby_3_0` (Ruby 3.0)

## Ruby Plugin Support

Devbox will automatically create the following configuration when you install Ruby with `devbox add`.

### Environment Variables

These environment variables configure Gem to install your gems locally, and set your Gem Home to a local folder

```bash
RUBY_CONFDIR={PROJECT_DIR}/.devbox/virtenv/ruby
GEMRC={PROJECT_DIR}/.devbox/virtenv/ruby/.gemrc
GEM_HOME={PROJECT_DIR}/.devbox/virtenv/ruby
```

### Init Hook

This hook ensures that your locally installed Gems are in your PATH while running `devbox shell`

```bash
"export PATH=\"{{ .Virtenv }}/bin:$PATH\""
```
