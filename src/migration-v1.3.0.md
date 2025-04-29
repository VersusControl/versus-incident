## Migration Guide to v1.3.0

This guide explains the changes introduced in Versus Incident v1.3.0 and how to update your configuration to take advantage of the new features.

### Key Changes in v1.3.0

Version 1.3.0 introduces enhanced on-call management capabilities and configuration options, with a focus on flexibility and team-specific routing.

#### 1. New Provider Configuration (Major Change from v1.2.0)

A significant change in v1.3.0 is the introduction of the `provider` property in the on-call configuration, which allows you to explicitly specify which on-call service to use:

```yaml
oncall:
  enable: false
  wait_minutes: 3
  provider: aws_incident_manager  # NEW in v1.3.0: Explicitly select "aws_incident_manager" or "pagerduty"
```

This change enables Versus Incident to support multiple on-call providers simultaneously. In v1.2.0, there was no provider selection mechanism, as AWS Incident Manager was the only supported provider.

#### 2. PagerDuty Integration (New in v1.3.0)

Version 1.3.0 introduces PagerDuty as a new on-call provider with comprehensive configuration options:

```yaml
oncall:
  provider: pagerduty  # Select PagerDuty as your provider

  pagerduty:  # New configuration section in v1.3.0
    routing_key: ${PAGERDUTY_ROUTING_KEY}  # Integration/Routing key for Events API v2
    other_routing_keys:  # Optional team-specific routing keys
      infra: ${PAGERDUTY_OTHER_ROUTING_KEY_INFRA}
      app: ${PAGERDUTY_OTHER_ROUTING_KEY_APP}
      db: ${PAGERDUTY_OTHER_ROUTING_KEY_DB}
```

The PagerDuty integration supports:
- Default routing key for general alerts
- Team-specific routing keys via the `other_routing_keys` configuration
- Dynamic routing using the `pagerduty_other_routing_key` query parameter

Example API call to target the infrastructure team:
```bash
curl -X POST "http://your-versus-host:3000/api/incidents?pagerduty_other_routing_key=infra" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] Load balancer failure.",
    "ServiceName": "lb-service",
    "UserID": "U12345"
  }'
```

#### 3. AWS Incident Manager Environment-Specific Response Plans (New in v1.3.0)

Version 1.3.0 enhances AWS Incident Manager integration with support for environment-specific response plans:

```yaml
oncall:
  provider: aws_incident_manager

  aws_incident_manager:
    response_plan_arn: ${AWS_INCIDENT_MANAGER_RESPONSE_PLAN_ARN}  # Default response plan
    other_response_plan_arns:  # New in v1.3.0
      prod: ${AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_PROD}
      dev: ${AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_DEV}
      staging: ${AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_STAGING}
```

This feature allows you to:
- Configure multiple response plans for different environments
- Dynamically select the appropriate response plan using the `awsim_other_response_plan` query parameter
- Use a more flexible named environment approach for response plan selection

Example API call to use the production environment's response plan:
```bash
curl -X POST "http://your-versus-host:3000/api/incidents?awsim_other_response_plan=prod" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] Production database failure.",
    "ServiceName": "prod-db-service",
    "UserID": "U12345"
  }'
```

### How to Migrate from v1.2.0

If you're upgrading from v1.2.0, update your on-call configuration to include the `provider` property.

### Complete Configuration Example

Replace your existing on-call configuration with the new structure:

```yaml
oncall:
  enable: false  # Set to true to enable on-call functionality
  wait_minutes: 3  # Time to wait for acknowledgment before escalating
  provider: aws_incident_manager  # or "pagerduty"

  aws_incident_manager:  # Used when provider is "aws_incident_manager"
    response_plan_arn: ${AWS_INCIDENT_MANAGER_RESPONSE_PLAN_ARN}
    other_response_plan_arns:  # NEW in v1.3.0: Optional environment-specific response plan ARNs
      prod: ${AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_PROD}
      dev: ${AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_DEV}
      staging: ${AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_STAGING}

  pagerduty:  # Used when provider is "pagerduty"
    routing_key: ${PAGERDUTY_ROUTING_KEY}
    other_routing_keys:  # Optional team-specific routing keys
      infra: ${PAGERDUTY_OTHER_ROUTING_KEY_INFRA}
      app: ${PAGERDUTY_OTHER_ROUTING_KEY_APP}
      db: ${PAGERDUTY_OTHER_ROUTING_KEY_DB}

redis:  # Required for on-call functionality
  host: ${REDIS_HOST}
  port: ${REDIS_PORT}
  password: ${REDIS_PASSWORD}
  db: 0
```

### Upgrading from v1.2.0

1. Update your Versus Incident deployment to v1.3.0:
   ```bash
   # Docker
   docker pull ghcr.io/versuscontrol/versus-incident:v1.3.0
   
   # Or update your Kubernetes deployment to use the new image
   ```

2. Update your configuration as described above, ensuring that Redis is properly configured if you're using on-call features.

3. Restart your Versus Incident service to apply the changes.

For any issues with the migration, please [open an issue](https://github.com/VersusControl/versus-incident/issues) on GitHub.