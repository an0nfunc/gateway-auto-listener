# Contributing to gateway-auto-listener

Thank you for your interest in contributing!

## Development Setup

1. **Prerequisites**: Go 1.24+, Docker, `just`, `helm`
2. **Clone**: `git clone https://github.com/an0nfunc/gateway-auto-listener.git`
3. **Dependencies**: `just deps`
4. **Build**: `just build`
5. **Test**: `just test`

## Running Tests

```bash
just test
```

## Linting

```bash
just lint
```

Requires [golangci-lint](https://golangci-lint.run/usage/install/).

## Pull Request Process

1. Fork the repository and create a feature branch from `main`.
2. Ensure all tests pass and linting is clean.
3. Write a clear commit message describing the change.
4. Open a PR against `main` with a description of what and why.
5. Address review feedback.

## Reporting Issues

Open an issue on GitHub with:
- What you expected to happen
- What actually happened
- Steps to reproduce
- Controller version and Kubernetes version
