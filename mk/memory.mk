# Compatibility wrapper for legacy "make -f mk/memory.mk ..." workflows.
# Canonical orchestration targets now live in the root Makefile.

.RECIPEPREFIX := >
SHELL := /bin/bash
ROOT_MAKEFILE := Makefile

.PHONY: help mem mem-up mem-restart mem-down mem-ps mem-logs mem-logs-short mem-ping \
	mem-mode-show mem-mode-core mem-mode-balanced mem-mode-full \
	mem-up-core mem-up-balanced mem-up-full observability-up observability-down

help:
> @echo "Legacy memory wrapper: forwards to ./$(ROOT_MAKEFILE)"
> @echo "Examples:"
> @echo "  make -f mk/memory.mk mem-up"
> @echo "  make -f mk/memory.mk mem-up-balanced"
> @echo "  make -f mk/memory.mk mem-mode-full"

mem-up:
> @$(MAKE) -f $(ROOT_MAKEFILE) up

mem-restart:
> @$(MAKE) -f $(ROOT_MAKEFILE) down
> @$(MAKE) -f $(ROOT_MAKEFILE) up

mem-down:
> @$(MAKE) -f $(ROOT_MAKEFILE) down

mem-ps:
> @$(MAKE) -f $(ROOT_MAKEFILE) ps

mem-logs:
> @$(MAKE) -f $(ROOT_MAKEFILE) logs

mem-logs-short:
> @$(MAKE) -f $(ROOT_MAKEFILE) logs

mem-ping:
> @$(MAKE) -f $(ROOT_MAKEFILE) mem-ping

mem-mode-show:
> @$(MAKE) -f $(ROOT_MAKEFILE) mem-mode-show

mem-mode-core:
> @$(MAKE) -f $(ROOT_MAKEFILE) mem-mode-core

mem-mode-balanced:
> @$(MAKE) -f $(ROOT_MAKEFILE) mem-mode-balanced

mem-mode-full:
> @$(MAKE) -f $(ROOT_MAKEFILE) mem-mode-full

mem-up-core:
> @$(MAKE) -f $(ROOT_MAKEFILE) mem-up-core

mem-up-balanced:
> @$(MAKE) -f $(ROOT_MAKEFILE) mem-up-balanced

mem-up-full:
> @$(MAKE) -f $(ROOT_MAKEFILE) mem-up-full

observability-up:
> @$(MAKE) -f $(ROOT_MAKEFILE) observability-up

observability-down:
> @$(MAKE) -f $(ROOT_MAKEFILE) observability-down

mem: mem-restart mem-ping mem-ps
