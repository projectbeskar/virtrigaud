# Security Policy

## Supported Versions

We actively support the following versions of Virtrigaud with security updates:

| Version | Supported          |
| ------- | ------------------ |
| 0.1.x   | :white_check_mark: |
| < 0.1   | :x:                |

## Reporting a Vulnerability

The Virtrigaud team takes security vulnerabilities seriously. We appreciate your efforts to responsibly disclose your findings, and will make every effort to acknowledge your contributions.

### How to Report

**Please do not report security vulnerabilities through public GitHub issues.**

Instead, please send an email to security@virtrigaud.io with the following information:

- A description of the vulnerability
- Steps to reproduce the issue
- Potential impact
- Any possible mitigations you've identified

You should receive a response within 48 hours. If for some reason you do not, please follow up via email to ensure we received your original message.

### What to Expect

- **Acknowledgment**: We will acknowledge receipt of your vulnerability report within 48 hours.
- **Assessment**: We will assess the vulnerability and determine its severity within 5 business days.
- **Mitigation**: For confirmed vulnerabilities, we will work on a fix and coordinate disclosure timeline with you.
- **Recognition**: We will credit you in our security advisory and release notes (unless you prefer to remain anonymous).

### Disclosure Policy

- We ask that you do not publicly disclose the vulnerability until we have had a chance to address it.
- We will coordinate with you on an appropriate disclosure timeline.
- We typically aim to disclose within 90 days of initial report.

## Security Considerations

### General Security

- Virtrigaud runs with minimal privileges and follows security best practices
- All communications with providers use TLS encryption
- Sensitive data (credentials, user data) is properly handled and never logged
- RBAC is enforced to limit access to resources

### Supply Chain Security

- All container images are signed with Cosign
- Software Bill of Materials (SBOM) is provided for all releases
- Container images are scanned for vulnerabilities
- Dependencies are regularly updated

### Network Security

- Network policies are provided to restrict traffic
- mTLS is supported for provider communications
- No unnecessary ports are exposed

### Access Control

- RBAC roles follow principle of least privilege
- Service accounts are properly scoped
- Admission webhooks enforce security policies

## Vulnerability Management

### Scanning

We regularly scan our codebase and dependencies for known vulnerabilities using:

- GitHub Security Advisories
- Trivy for container scanning
- Go vulnerability database
- OWASP dependency checking

### Response Process

1. **Detection**: Vulnerability discovered through scanning or reporting
2. **Assessment**: Determine severity and impact
3. **Patching**: Develop and test fix
4. **Release**: Create security release with patch
5. **Notification**: Inform users through security advisory

### Severity Classification

We use the following severity levels:

- **Critical**: Immediate action required, patch within 24 hours
- **High**: Patch within 7 days
- **Medium**: Patch within 30 days
- **Low**: Patch in next regular release

## Security Features

### Authentication and Authorization

- Integration with Kubernetes RBAC
- Support for external identity providers
- Service account token projection
- Webhook authentication

### Encryption

- TLS 1.2+ for all communications
- Certificate rotation and management
- Support for custom CA certificates
- Secrets encryption at rest (Kubernetes level)

### Audit and Monitoring

- Comprehensive audit logging
- Security event monitoring
- Metrics for security-relevant events
- Integration with security monitoring tools

## Best Practices for Users

### Deployment Security

1. **Use namespace isolation**: Deploy in dedicated namespace
2. **Apply network policies**: Restrict network access
3. **Enable Pod Security Standards**: Use strict or baseline profiles
4. **Regular updates**: Keep Virtrigaud and dependencies updated
5. **Monitor security advisories**: Subscribe to security notifications

### Credential Management

1. **Use external secret management**: HashiCorp Vault, External Secrets Operator
2. **Rotate credentials regularly**: Implement credential rotation
3. **Principle of least privilege**: Grant minimal required permissions
4. **Secure storage**: Never store credentials in Git or plain text

### Network Security

1. **Enable TLS**: Use TLS for all communications
2. **Network segmentation**: Isolate provider networks
3. **Firewall rules**: Restrict hypervisor access
4. **VPN access**: Use VPN for remote hypervisor access

### Monitoring and Alerting

1. **Security monitoring**: Monitor for security events
2. **Failed authentication alerts**: Alert on authentication failures
3. **Unusual activity**: Monitor for unexpected behavior
4. **Compliance scanning**: Regular security scans

## Compliance

Virtrigaud is designed to support compliance with various security frameworks:

- **SOC 2**: Control implementation guidance available
- **ISO 27001**: Security control mapping provided
- **CIS Kubernetes Benchmark**: Alignment with security benchmarks
- **NIST Cybersecurity Framework**: Control implementation guidance

## Security Tools and Integrations

### Supported Security Tools

- **Falco**: Runtime security monitoring
- **OPA Gatekeeper**: Policy enforcement
- **Twistlock/Prisma**: Container security scanning
- **Aqua Security**: Container and runtime security
- **Cilium**: Network security and observability

### Security Configurations

Example security-hardened configurations are provided in:

- `examples/security/strict-rbac.yaml`
- `examples/security/network-policies.yaml`
- `examples/security/pod-security-policies.yaml`
- `examples/security/external-secrets.yaml`

## Contact

For security-related questions that are not vulnerabilities, you can:

- Open a GitHub Discussion in the Security category
- Email security@virtrigaud.io
- Join the #virtrigaud-security channel on Kubernetes Slack

## Recognition

We maintain a security hall of fame for researchers who have helped improve Virtrigaud security:

- [Security Contributors](docs/security/contributors.md)

Thank you to all the security researchers who have contributed to making Virtrigaud more secure!
