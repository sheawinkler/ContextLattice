BASH_AVAILABLE := $(shell test -x /bin/bash && echo yes)
ifeq ($(BASH_AVAILABLE),yes)
  SHELL := /bin/bash
  .SHELLFLAGS := -eu -o pipefail -c
else
  SHELL := /bin/sh
  .SHELLFLAGS := -eu -c
endif
.ONESHELL:
