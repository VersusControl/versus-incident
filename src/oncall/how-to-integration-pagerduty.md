## How to Integrate with PagerDuty

## Table of Contents
- [Prerequisites](#prerequisites)
- [Setting Up PagerDuty for On-Call](#setting-up-pagerduty-for-on-call)
  - [1. Create Users in PagerDuty](#1-create-users-in-pagerduty)
  - [2. Create On-Call Schedules](#2-create-on-call-schedules)
  - [3. Create Escalation Policies](#3-create-escalation-policies)
  - [4. Create a PagerDuty Service](#4-create-a-pagerduty-service)
  - [5. Get the Integration Key](#5-get-the-integration-key)
- [Deploy Versus Incident](#deploy-versus-incident)
  - [Docker Deployment](#docker-deployment)
- [Alert Manager Routing Configuration](#alert-manager-routing-configuration)
- [Override the PagerDuty Routing Key per Alert](#override-the-pagerduty-routing-key-per-alert)
- [Testing the Integration](#testing-the-integration)
- [How It Works Under the Hood](#how-it-works-under-the-hood)
- [Conclusion](#conclusion)

This document provides a step-by-step guide to integrate Versus Incident with PagerDuty for on-call management. The integration enables automated escalation of alerts to on-call teams when incidents are not acknowledged within a specified time.

We'll cover setting up PagerDuty, configuring the integration with Versus, deploying Versus, and testing the integration with practical examples.

### Prerequisites

Before you begin, ensure you have:
+ A PagerDuty account (you can start with a free trial if needed)
+ Versus Incident deployed (instructions provided later)
+ Prometheus Alert Manager set up to monitor your systems

### Setting Up PagerDuty for On-Call

Let's configure a practical example in PagerDuty with services, schedules, and escalation policies.

#### 1. Create Users in PagerDuty

First, we need to set up the users who will be part of the on-call rotation:

1. Log in to your PagerDuty account
2. Navigate to **People** > **Users** > **Add User**
3. For each user, enter:
   + Name (e.g., "Natsu Dragneel")
   + Email address
   + Role (User)
   + Time Zone
4. PagerDuty will send an email invitation to each user
5. Users should complete their profiles by:
   + Setting up notification rules (SMS, email, push notifications)
   + Downloading the PagerDuty mobile app
   + Setting contact details

Repeat to create multiple users (e.g., Natsu, Zeref, Igneel, Gray, Gajeel, Laxus).

#### 2. Create On-Call Schedules

Now, let's create schedules for who is on-call and when:

1. Navigate to **People** > **Schedules** > **Create Schedule**
2. Name the schedule (e.g., "Team A Primary")
3. Set up the rotation:
   + Choose a rotation type (Weekly is common)
   + Add users to the rotation (e.g., Natsu, Zeref, Igneel)
   + Set handoff time (e.g., Mondays at 9:00 AM)
   + Set time zone
4. Save the schedule
5. Create a second schedule (e.g., "Team B Secondary") for other team members

#### 3. Create Escalation Policies

An escalation policy defines who gets notified when an incident occurs:

1. Navigate to **Configuration** > **Escalation Policies** > **New Escalation Policy**
2. Name the policy (e.g., "Critical Incident Response")
3. Add escalation rules:
   + Level 1: Select the "Team A Primary" schedule with a 5-minute timeout
   + Level 2: Select the "Team B Secondary" schedule
   + Optionally, add a Level 3 to notify a manager
4. Save the policy

#### 4. Create a PagerDuty Service

A service is what receives incidents from monitoring systems:

1. Navigate to **Configuration** > **Services** > **New Service**
2. Name the service (e.g., "Versus Incident Integration")
3. Select "Events API V2" as the integration type
4. Select the escalation policy you created in step 3
5. Configure incident settings (Auto-resolve, urgency, etc.)
6. Save the service

#### 5. Get the Integration Key

After creating the service, you'll need the integration key (also called routing key):

1. Navigate to **Configuration** > **Services**
2. Click on your newly created service
3. Go to the **Integrations** tab
4. Find the "Events API V2" integration
5. Copy the **Integration Key** (it looks something like: `12345678abcdef0123456789abcdef0`)
6. Keep this key secure - you'll need it for Versus configuration

### Deploy Versus Incident

Now let's deploy Versus with PagerDuty integration. You can use Docker or Kubernetes.

#### Docker Deployment

Create a directory for your configuration files:

```bash
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

oncall:
  enable: true
  wait_minutes: 3
  provider: pagerduty

  pagerduty:
    routing_key: ${PAGERDUTY_ROUTING_KEY} # The Integration Key from step 5
    other_routing_keys:
      infra: ${PAGERDUTY_OTHER_ROUTING_KEY_INFRA}
      app: ${PAGERDUTY_OTHER_ROUTING_KEY_APP}
      db: ${PAGERDUTY_OTHER_ROUTING_KEY_DB}

redis: # Required for on-call functionality
  insecure_skip_verify: true # dev only
  host: ${REDIS_HOST}
  port: ${REDIS_PORT}
  password: ${REDIS_PASSWORD}
  db: 0
```

Create a Slack template in `config/slack_message.tmpl`:

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

**About the ACK URL Generation**

+ When an incident is created (e.g., via a POST to `/api/incidents`), Versus generates an acknowledgment URL if on-call is enabled.
+ The URL is constructed using the `public_host` value: `https://your-host.example/api/incidents/ack/<incident-id>`.
+ This URL is injected into the alert data as `.AckURL` for use in templates.

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
      - PAGERDUTY_ROUTING_KEY=your_pagerduty_integration_key
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

Run Docker Compose:

```bash
docker-compose up -d
```

### Alert Manager Routing Configuration

Now, let's configure Alert Manager to route alerts to Versus with different behaviors:

#### Send Alert Only (No On-Call)

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

#### Send Alert with Acknowledgment Wait

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

This configuration waits 5 minutes for acknowledgment before triggering PagerDuty if the user doesn't click the ACK link in Slack.

#### Send Alert with Immediate On-Call Trigger

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

This will trigger PagerDuty immediately without waiting.

### Override the PagerDuty Routing Key per Alert

You can configure Alert Manager to use different PagerDuty services for specific alerts by using named routing keys instead of exposing sensitive routing keys directly in URLs:

```yaml
receivers:
- name: 'versus-pagerduty-infra'
  webhook_configs:
  - url: 'http://versus-service:3000/api/incidents?pagerduty_other_routing_key=infra'
    send_resolved: false

route:
  routes:
  - match:
      team: infrastructure
    receiver: 'versus-pagerduty-infra'
```

This routes infrastructure team alerts to a different PagerDuty service using the named routing key "infra", which is securely mapped to the actual integration key in your configuration file:

```yaml
oncall:
  provider: pagerduty
  pagerduty:
    routing_key: ${PAGERDUTY_ROUTING_KEY}
    other_routing_keys:
      infra: ${PAGERDUTY_OTHER_ROUTING_KEY_INFRA}
      app: ${PAGERDUTY_OTHER_ROUTING_KEY_APP}
      db: ${PAGERDUTY_OTHER_ROUTING_KEY_DB}
```

This approach keeps your sensitive PagerDuty integration keys secure by never exposing them in URLs or logs.

### Testing the Integration

Let's test the complete workflow:

1. **Trigger an Alert**:
   - Simulate a critical alert in Prometheus to match the Alert Manager rule.

2. **Verify Versus**:
   - Check that Versus receives the alert and sends it to Slack.
   - You should see a message with an acknowledgment link.

3. **Check Escalation**:
   - Option 1: Click the ACK link to acknowledge the incident - PagerDuty should not be notified.
   - Option 2: Wait for the acknowledgment timeout (e.g., 5 minutes) without clicking the link.
   - In PagerDuty, verify that an incident is created and the on-call person is notified.
   - Confirm that escalation happens according to your policy if the incident remains unacknowledged.

4. **Immediate Trigger Test**:
   - Send an urgent alert and confirm that PagerDuty is triggered instantly.

### How It Works Under the Hood

When Versus integrates with PagerDuty, the following occurs:

1. Versus receives an alert from Alert Manager
2. If on-call is enabled and the acknowledgment period passes without an ACK, Versus:
   - Constructs a PagerDuty Events API v2 payload
   - Sends a "trigger" event to PagerDuty with your routing key
   - Includes incident details as custom properties

The PagerDuty service processes this event according to your escalation policy, notifying the appropriate on-call personnel.

### Conclusion

You've now integrated Versus Incident with PagerDuty for on-call management! Alerts from Prometheus Alert Manager can trigger notifications via Versus, with escalations handled by PagerDuty based on your escalation policy.

This integration provides:

+ A delay period for engineers to acknowledge incidents before escalation
+ Slack notifications with one-click acknowledgment
+ Structured escalation through PagerDuty's robust notification system
+ Multiple notification channels to ensure critical alerts reach the right people

Adjust configurations as needed for your environment and incident response processes. If you encounter any issues or have further questions, feel free to reach out!
