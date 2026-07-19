# Security Policy

## Reporting a Vulnerability

We take security vulnerabilities seriously. If you discover a security issue,
please report it responsibly.

### How to Report

1. **Do NOT create public GitHub issues** for security vulnerabilities
2. **GitHub Security Advisories (preferred)**: Report vulnerabilities privately via
   [GitHub Security Advisories](../../security/advisories/new)
3. **Email**: Send reports to **security@rossoctl.io**
4. **Include**: A clear description of the vulnerability, steps to reproduce,
   and potential impact

### Security Contacts

- **rossoctl-maintainers@googlegroups.com**
- **security@rossoctl.io**

### What to Expect

- We will acknowledge receipt within 48 hours
- We aim to provide an initial assessment within 7 days
- We will keep you informed of our progress
- We will credit you in the security advisory (if desired)

## Supported Versions

| Version | Supported          |
|---------|--------------------|
| main    | :white_check_mark: |

## Security Measures

This project implements several security controls:

- **CI/CD Security**: Lint, unit test, E2E test, and build checks on every PR
- **Dependency Scanning**: Automated updates via Dependabot for GitHub Actions
- **Code Quality**: golangci-lint with static analysis
- **Signed Commits**: All commits require sign-off (`git commit -s`)

## Operator-Specific Security

The rossoctl-operator manages trusted workload identity and agent card signatures:

- **SPIFFE/SPIRE Integration**: Workload identity via x509-SVIDs
- **Agent Card Signing**: JWS signatures with x5c certificate chains (A2A spec §8.4.2)
- **Identity Binding**: Trust domain validation for agent workloads
- **Network Policies**: Automatic isolation for agents with failed identity binding
