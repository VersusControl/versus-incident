## How to Integration

## Table of Contents
- [Prerequisites](#prerequisites)
- [Setting Up AWS Incident Manager for On-Call](#setting-up-aws-incident-manager-for-on-call)
- [Define IAM Role for Versus](#define-iam-role-for-versus)
- [Deploy Versus Incident](#deploy-versus-incident)
- [Alert Rules](#alert-rules)
- [Alert Manager Routing Configuration](#alert-manager-routing-configuration)
- [Testing the Integration](#testing-the-integration)
- [Conclusion](#conclusion)

This document provides a step-by-step guide to integrate Versus Incident with AWS Incident Manager make an On Call. The integration enables automated escalation of alerts to on-call teams when incidents are not acknowledged within a specified time.

We'll cover configuring Prometheus Alert Manager to send alerts to Versus, setting up AWS Incident Manager, deploying Versus, and testing the integration with a practical example.

### Prerequisites

Before you begin, ensure you have:
+ An AWS account with access to AWS Incident Manager.
+ Versus Incident deployed (instructions provided later).
+ Prometheus Alert Manager set up to monitor your systems.

### Setting Up AWS Incident Manager for On-Call

AWS Incident Manager requires configuring several components to manage on-call workflows. Let’s configure a practical example using 6 contacts, two teams, and a two-stage response plan. Use the AWS Console to set these up. 

**Contacts**

Contacts are individuals who will be notified during an incident.

1. In the AWS Console, navigate to Systems Manager > Incident Manager > Contacts.
2. Click Create contact.
3. For each contact:
+ Enter a Name (e.g., "Natsu Dragneel").
+ Add Contact methods (e.g., SMS: +1-555-123-4567, Email: natsu@devopsvn.tech).
+ Save the contact.

Repeat to create 6 contacts (e.g., Natsu, Zeref, Igneel, Gray, Gajeel, Laxus).

**Escalation Plan**

An escalation plan defines the order in which contacts are engaged.
1. Go to Incident Manager > Escalation plans > Create escalation plan.
2. Name it (e.g., "TeamA_Escalation").
3. Add contacts (e.g., Natsu, Zeref, and Igneel) and set them to engage simultaneously or sequentially.
4. Save the plan.
5. Create a second plan (e.g., "TeamB_Escalation") for Gray, Gajeel, and Laxus.

**RunBook (Optional)**

RunBooks automate incident resolution steps. For this guide, we’ll skip RunBook creation, but you can define one in AWS Systems Manager Automation if needed.

**Response Plan**

A response plan ties contacts and escalation plans into a structured response.
1. Go to Incident Manager > Response plans > Create response plan.
2. Name it (e.g., "CriticalIncidentResponse").
3. Choose an **Escalation Plan** we had created, which defines two stages:
+ Stage 1: Engage "TeamA_Escalation" (Natsu, Zeref, and Igneel) with a 5-minute timeout.
+ Stage 2: If unacknowledged, engage "TeamB_Escalation" (Gray, Gajeel, and Laxus).
4. Save the plan and note its ARN (e.g., `arn:aws:ssm-incidents::111122223333:response-plan/CriticalIncidentResponse`).

### Define IAM Role for Versus

Versus needs permissions to interact with AWS Incident Manager.

1. In the AWS Console, go to IAM > Roles > Create role.
2. Choose AWS service as the trusted entity and select EC2 (or your deployment type, e.g., ECS).
3. Attach a custom policy with these permissions:
```json
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Action": [
                "ssm-incidents:StartIncident",
                "ssm-incidents:GetResponsePlan"
            ],
            "Resource": "*"
        }
    ]
}
```
4. Name the role (e.g., "VersusIncidentRole") and create it.
5. Note the Role ARN (e.g., `arn:aws:iam::111122223333:role/VersusIncidentRole`).

### Deploy Versus Incident

Deploy Versus using Docker or Kubernetes. Docker Deployment. Create a directory for your configuration files:

```
mkdir -p ./config
```

Create `config/config.yaml` with the following content:

```yaml
name: versus
host: 0.0.0.0
port: 3000
public_host: https://your-ack-host.example

alert:
  debug_body: true

  slack:
    enable: true
    token: ${SLACK_TOKEN}
    channel_id: ${SLACK_CHANNEL_ID}
    template_path: "config/slack_message.tmpl"
    message_properties:
      button_text: "Acknowledge Alert" # Custom text for the acknowledgment button
      button_style: "primary" # Button style: "primary" (blue), "danger" (red), or empty for default gray
      disable_button: false # Set to true to disable the button if you want to handle acknowledgment differently

oncall:
  enable: true
  wait_minutes: 3

  aws_incident_manager:
    response_plan_arn: ${AWS_INCIDENT_MANAGER_RESPONSE_PLAN_ARN}

redis: # Required for on-call functionality
  insecure_skip_verify: true # dev only
  host: ${REDIS_HOST}
  port: ${REDIS_PORT}
  password: ${REDIS_PASSWORD}
  db: 0
```

Create Slack templates `config/slack_message.tmpl`:

```
🔥 *{{ .commonLabels.severity | upper }} Alert: {{ .commonLabels.alertname }}*

🌐 *Instance*: `{{ .commonLabels.instance }}`
🚨 *Status*: `{{ .status }}`

{{ range .alerts }}
📝 {{ .annotations.description }}  
{{ end }}
```

**Slack Acknowledgment Button (Default)**

By default, Versus automatically adds an interactive acknowledgment button to Slack notifications when on-call is enabled. This allows users to acknowledge alerts. You can customize the button appearance in your `config.yaml`, for example:

![Versus On-Call Slack](/docs/images/on-call-slack.png)

**ACK URL Generation**

+ When an incident is created (e.g., via a POST to `/api/incidents`), Versus generates an acknowledgment URL if on-call is enabled.
+ The URL is constructed using the `public_host` value, typically in the format: `https://your-host.example/api/incidents/ack/<incident-id>`.
+ This URL is injected into the button.

**Manual Acknowledgment Handling**

If you prefer to handle acknowledgments manually or want to disable the default button (by setting `disable_button: true`), you can add the acknowledgment URL directly in your template. Here's an example of including a clickable link in your Slack template:

```
🔥 *{{ .commonLabels.severity | upper }} Alert: {{ .commonLabels.alertname }}*

🌐 *Instance*: `{{ .commonLabels.instance }}`  
🚨 *Status*: `{{ .status }}`

{{ range .alerts }}
📝 {{ .annotations.description }}  
{{ end }}
{{ if .AckURL }}
----------
<{{.AckURL}}|Click here to acknowledge>
{{ end }}
```

The conditional `{{ if .AckURL }}` ensures the link only appears if the acknowledgment URL is available (i.e., when on-call is enabled).

Create the `docker-compose.yml` file:

```yaml
version: '3.8'

services:
  versus:
    image: ghcr.io/versuscontrol/versus-incident
    ports:
      - "3000:3000"
    environment:
      - SLACK_TOKEN=your_slack_token
      - SLACK_CHANNEL_ID=your_channel_id
      - AWS_INCIDENT_MANAGER_RESPONSE_PLAN_ARN=arn:aws:ssm-incidents::111122223333:response-plan/CriticalIncidentResponse
      - REDIS_HOST=redis
      - REDIS_PORT=6379
      - REDIS_PASSWORD=your_redis_password
    depends_on:
      - redis

  redis:
    image: redis:6.2-alpine
    command: redis-server --requirepass your_redis_password
    ports:
      - "6379:6379"
    volumes:
      - redis_data:/data

volumes:
  redis_data:
```

*Note: If using AWS credentials, add `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` environment variables, or attach the IAM role to your deployment environment.*

Run Docker Compose:

```bash
docker-compose up -d
```

### Alert Rules

Create a `prometheus.yml` file to define a metric and alerting rule:

```yaml
global:
  scrape_interval: 15s

scrape_configs:
  - job_name: 'server'
    static_configs:
      - targets: ['localhost:9090']

rule_files:
  - 'alert_rules.yml'
```

Create `alert_rules.yml` to define an alert:

```yaml
groups:
- name: rate
  rules:

  - alert: HighErrorRate
    expr: rate(http_requests_total{status="500"}[5m]) > 0.1
    for: 5m
    labels:
      severity: warning
    annotations:
      summary: "High error rate detected in {{ $labels.service }}"
      description: "{{ $labels.service }} has an error rate above 0.1 for 5 minutes."

  - alert: HighErrorRate
    expr: rate(http_requests_total{status="500"}[5m]) > 0.5
    for: 2m
    labels:
      severity: critical
    annotations:
      summary: "Very high error rate detected in {{ $labels.service }}"
      description: "{{ $labels.service }} has an error rate above 0.5 for 2 minutes."

  - alert: HighErrorRate
    expr: rate(http_requests_total{status="500"}[5m]) > 0.8
    for: 1m
    labels:
      severity: urgent
    annotations:
      summary: "Extremely high error rate detected in {{ $labels.service }}"
      description: "{{ $labels.service }} has an error rate above 0.8 for 1 minute."
```

### Alert Manager Routing Configuration

Configure Alert Manager to route alerts to Versus with different behaviors.

**Send Alert Only (No On-Call)**

```yaml
receivers:
- name: 'versus-no-oncall'
  webhook_configs:
  - url: 'http://versus-service:3000/api/incidents?oncall_enable=false'
    send_resolved: false

route:
  receiver: 'versus-no-oncall'
  group_by: ['alertname', 'service']
  routes:
  - match:
      severity: warning
    receiver: 'versus-no-oncall'
```

**Send Alert with Acknowledgment Wait**

```yaml
receivers:
- name: 'versus-with-ack'
  webhook_configs:
  - url: 'http://versus-service:3000/api/incidents?oncall_wait_minutes=5'
    send_resolved: false

route:
  routes:
  - match:
      severity: critical
    receiver: 'versus-with-ack'
```

This waits 5 minutes for acknowledgment before triggering the AWS Incident Manager Response Plan if the user doesn't click the link ACK that Versus sends to Slack.

**Send Alert with Immediate On-Call Trigger**

```yaml
receivers:
- name: 'versus-immediate'
  webhook_configs:
  - url: 'http://versus-service:3000/api/incidents?oncall_wait_minutes=0'
    send_resolved: false

route:
  routes:
  - match:
      severity: urgent
    receiver: 'versus-immediate'
```

This triggers the response plan immediately without waiting.

### Testing the Integration

1. Trigger an Alert: Simulate a critical alert in Prometheus to match the Alert Manager rule.
2. Verify Versus: Check that Versus receives the alert and sends it to configured channels (e.g., Slack).
3. Check Escalation:
+ Wait 5 minutes without acknowledging the alert.
+ In Incident Manager > Incidents, verify that an incident starts and Team A is engaged.
+ After 5 more minutes, confirm Team B is engaged.
4. Immediate Trigger Test: Send an urgent alert and confirm the response plan triggers instantly.

Result

![Versus On-Call Result](/docs/images/on-call-result.png)

### Conclusion

You’ve now integrated Versus Incident with AWS Incident Manager for on-call management! Alerts from Prometheus Alert Manager can trigger notifications via Versus, with escalations handled by AWS Incident Manager based on your response plan. Adjust configurations as needed for your environment.

If you encounter any issues or have further questions, feel free to reach out!
