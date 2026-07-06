# Go SRE Agent Project Skeleton Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Create a compilable Go project skeleton for a framework-free SRE diagnostic agent.

**Architecture:** The skeleton defines stable package boundaries for runtime, LLM providers, tools, policy, trace, schema, report, and CLI. The first pass includes mock provider behavior and unit tests for the core loop boundaries, while concrete network tools remain a later milestone.

**Tech Stack:** Go 1.22, standard library only for the skeleton.

---

### Task 1: Core Boundaries

**Files:**
- Create: `go.mod`
- Create: `internal/schema/*.go`
- Create: `internal/tools/*.go`
- Create: `internal/llm/*.go`
- Create: `internal/policy/*.go`
- Create: `internal/trace/*.go`
- Create: `internal/agent/*.go`

- [x] Write failing tests for registry, policy, trace, and runtime behavior.
- [x] Run `GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go-mod go test ./...` and verify missing skeleton packages fail.
- [x] Add minimal implementations needed for the tests.

### Task 2: CLI And Documentation

**Files:**
- Create: `cmd/sre-agent/main.go`
- Create: `configs/config.example.yaml`
- Create: `docs/architecture.md`
- Create: `docs/examples.md`
- Create: `examples/chat_proj/*.md`
- Create: `README.md`

- [x] Add a CLI stub with `diagnose --goal`.
- [x] Add config and example documentation.
- [ ] Run gofmt and full tests before reporting status.
