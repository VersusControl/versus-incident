# Summary
[Introduction](./introduction.md)

# AI SRE Agent
- [Introduction](./agent/agent-introduction.md)
- [Getting Started](./agent/getting-started.md)
- [Configuration](./agent/configuration.md)
- [Data Sources](./agent/data-sources.md)
  - [File](./agent/data-sources/file.md)
  - [Elasticsearch](./agent/data-sources/elasticsearch.md)
  - [Loki](./agent/data-sources/loki.md)
  - [CloudWatch Logs](./agent/data-sources/cloudwatch-logs.md)
  - [Graylog](./agent/data-sources/graylog.md)
  - [Splunk](./agent/data-sources/splunk.md)
- [Notification Channels](./agent/channels.md)
  - [Slack](./agent/channels/slack.md)
  - [Microsoft Teams](./agent/channels/msteams.md)
  - [Telegram](./agent/channels/telegram.md)
  - [Viber](./agent/channels/viber.md)
  - [Email](./agent/channels/email.md)
  - [Lark](./agent/channels/lark.md)
- [Shadow Mode](./agent/shadow-mode.md)
- [Spike Detection](./agent/spike.md)
- [AI Detect Mode](./agent/ai-detect-mode.md)
- [AI Analyze Mode](./agent/ai-analyze-mode.md)
  - [Analyze Tools](./agent/analyze-tools/tools.md)
- [Redaction](./agent/redaction.md)
- [Catalog](./agent/catalog.md)
- [Miner](./agent/miner.md)
- [Regex](./agent/regex.md)

# Configuration
- [Overview](./configuration/admin-ui.md)
- [Configuration](./configuration/configuration.md)
- [Deploy on Kubernetes](./configuration/kubernetes.md)
- [Helm Chart](./configuration/helm.md)

# Notifications & Webhook Alerts
- [Getting Started](./webhook/getting-started.md)
- [Template Syntax](./webhook/template-syntax.md)
- [Advanced Template Tips](./webhook/advanced-template-tips.md)


# On Call
- [Introduction](./oncall/on-call-introduction.md)
- [AWS Incident Manager](./oncall/aws-incident-manager.md)
- [How to Integration Incident Manager](./oncall/how-to-integration-aws-icm.md)
- [How to Integration Incident Manager (Advanced)](./oncall/how-to-integration-aws-icm-adv.md)
- [PagerDuty](./oncall/pagerduty.md)
- [How to Integration PagerDuty](./oncall/how-to-integration-pagerduty.md)

# Webhook Integration Examples
- [Use Alertmanager](./examples/alertmanager.md)
- [Use FluentBit](./examples/fluent-bit.md)
- [Use CloudWatch Alarm](./examples/cloudwatch-alarm-sns.md)
- [Use Sentry](./examples/sentry.md)
- [Use Kibana](./examples/kibana.md)

# Migration Guides
- [Migrating to v1.2.0](./migration/migration-v1.2.0.md)
- [Migrating to v1.3.0](./migration/migration-v1.3.0.md)
- [Migrating to v1.4.0](./migration/migration-v1.4.0.md)
