## How to Integration AWS Incident Manager On-Call (Advanced)

## Table of Contents
- [Prerequisites](#prerequisites)
- [Advanced On-Call Management with AWS Incident Manager](#advanced-on-call-management-with-aws-incident-manager)
- [Creating On-Call Schedules](#creating-on-call-schedules)
- [Understanding AWS Incident Manager Rotations](#understanding-aws-incident-manager-rotations)
- [Multi-Level Escalation Workflows](#multi-level-escalation-workflows)
- [Advanced Versus Incident Configuration](#advanced-versus-incident-configuration)
- [AWS IAM Role Configuration for Critical-Only Approach](#aws-iam-role-configuration-for-critical-only-approach)
- [Advanced Incident Routing Rules](#advanced-incident-routing-rules)
- [Dynamic Configuration with Query Parameters](#dynamic-configuration-with-query-parameters)
- [Monitoring and Analytics](#monitoring-and-analytics)
- [Testing and Validation](#testing-and-validation)
- [Testing the Critical-Only Approach](#testing-the-critical-only-approach)
- [Conclusion](#conclusion)

This document provides an advanced guide to integrating Versus Incident with AWS Incident Manager for advanced on-call management. While the basic integration guide covers essential setup, this advanced guide focuses on implementing complex on-call rotations, schedules, and workflows.

### Prerequisites

Before proceeding with this advanced guide, ensure you have:
+ Completed the basic [AWS Incident Manager integration](/src/oncall/how-to-integration-aws-icm.md)
+ An AWS account with administrative access
+ Versus Incident deployed and functioning with basic integrations
+ Prometheus Alert Manager configured and sending alerts to Versus
+ Multiple teams requiring on-call management with different rotation patterns

### Advanced On-Call Management with AWS Incident Manager

AWS Incident Manager offers advanced capabilities for managing on-call schedules, beyond the basic escalation plans covered in the introductory guide. These include:

- **On-Call Schedules**: Calendar-based rotations of on-call responsibilities
- **Rotation Patterns**: Daily, weekly, or custom rotation patterns for teams
- **Time Zone Management**: Support for global teams across different time zones
- **Override Capabilities**: Handling vacations, sick leave, and special events

Let's configure an advanced on-call system with two teams (Platform and Application) that have different rotation schedules and escalation workflows.

### Creating On-Call Schedules

AWS Incident Manager allows you to create on-call schedules that automatically rotate responsibilities among team members. Here's how to set up comprehensive schedules:

1. **Create Team-Specific Contact Groups**:
   - In the AWS Console, navigate to **Systems Manager > Incident Manager > Contacts**
   - Click **Create contact group**
   - For the Platform team:
     + Name it "Platform-Team"
     + Add 4-6 team member contacts created previously
     + Save the group
   - Repeat for the Application team

2. **Create Schedule Rotations**:
   - Go to **Incident Manager > Schedules**
   - Click **Create schedule**
   - Configure the Platform team rotation:
     + **Name**: "Platform-Rotation"
     + **Description**: "24/7 support rotation for platform infrastructure"
     + **Time zone**: Select your primary operations time zone
     + **Rotation settings**:
       * **Rotation pattern**: Weekly (Each person is on call for 1 week)
       * **Start date/time**: Choose when the first rotation begins
       * **Handoff time**: Typically 09:00 AM local time
     + **Recurrence**: Recurring every 1 week
     + Add all platform engineers to the rotation sequence
     + Save the schedule

3. **Create Application Team Schedule With Daily Rotation**:
   - Create another schedule named "App-Rotation"
   - Configure for daily rotation instead of weekly
   - Set business hours coverage (8 AM - 6 PM)
   - Add application team members
   - Save the schedule

You now have two separate rotation schedules that will automatically change the primary on-call contact based on the defined patterns.

#### Understanding AWS Incident Manager Rotations

AWS Incident Manager rotations provide a powerful way to manage on-call responsibilities. Here's a deeper explanation of how they work:

1. **Rotation Sequence Management**:
   - Engineers are added to the rotation in a specific sequence
   - Each engineer takes their turn as the primary on-call responder based on the configured rotation pattern
   - AWS automatically tracks the current position in the rotation and advances it according to the schedule

2. **Shift Transition Process**:
   - At the configured handoff time (e.g., 9:00 AM), AWS Incident Manager automatically transitions on-call responsibilities
   - The system sends notifications to both the outgoing and incoming on-call engineers
   - The previous on-call engineer remains responsible until the handoff is complete
   - Any incidents created during the handoff window are assigned to the new on-call engineer

3. **Handling Availability Exceptions**:
   - AWS Incident Manager allows you to create **Overrides** for planned absences like vacations or holidays
   - To create an override:
     + Navigate to the schedule
     + Click "Create override"
     + Select the time period and replacement contact
     + Save the override
   - During the override period, notifications are sent to the replacement contact instead of the regularly scheduled engineer
   
4. **Multiple Rotation Layers**:
   - You can create primary, secondary, and tertiary rotation schedules
   - These can be combined into escalation plans where notification fails over from primary to secondary
   - Different rotations can have different time periods (e.g., primary rotates weekly, secondary rotates monthly)
   - This adds redundancy to your on-call system and spreads the on-call burden appropriately

5. **Managing Time Zones and Global Teams**:
   - AWS Incident Manager handles time zone differences automatically
   - You can configure a "Follow-the-Sun" rotation where engineers in different time zones cover different parts of the day
   - The handoff times are adjusted based on the configured time zone of the schedule

6. **Rotation Visualization**:
   - The AWS Console provides a calendar view that shows who is on-call at any given time
   - This helps teams plan their schedules and understand upcoming on-call responsibilities
   - The calendar view accounts for overrides and exceptions

### Multi-Level Escalation Workflows

Build advanced escalation workflows that incorporate your on-call schedules:

1. **Create Advanced Escalation Plans**:
   - Go to **Incident Manager > Escalation plans**
   - Click **Create escalation plan**
   - Name it "Platform-Tiered-Escalation"
   - Add escalation stages:
     + **Stage 1**: Current on-call from "Platform-Rotation" (wait 5 minutes)
     + **Stage 2**: Secondary on-call + Team Lead (wait 5 minutes)
     + **Stage 3**: Engineering Manager + Director (wait 10 minutes)
     + **Stage 4**: CTO/VP Engineering

2. **Configure Severity-Based Escalation**:
   Create an escalation plan specifically for critical alerts:
   - Critical: Immediate engagement of primary on-call, with fast escalation (2-minute acknowledgment)
   - Note: Non-critical alerts don't trigger on-call processes

3. **Create Enhanced Response Plans**:
   - Go to **Incident Manager > Response plans**
   - Create separate response plans aligned with different services and severity levels
   - For example, "Critical-Platform-Outage" with:
     + Associated escalation plan: "Platform-Tiered-Escalation"
     + Automatic engagement of specific chat channels
     + Pre-defined runbooks for common failure scenarios
     + Integration with status page updates

These advanced escalation workflows ensure that the right people are engaged at the right time, without unnecessary escalation for routine issues.

### Advanced Versus Incident Configuration

Configure Versus Incident for advanced integration with AWS Incident Manager:

```yaml
name: versus
host: 0.0.0.0
port: 3000
public_host: https://versus.example.com  # Required for acknowledgment URLs

alert:
  debug_body: true  # Useful for troubleshooting

  slack:
    enable: true
    token: ${SLACK_TOKEN}
    channel_id: ${SLACK_CHANNEL_ID}
    template_path: "config/slack_message.tmpl"
    message_properties:
      button_text: "Acknowledge Incident"
      button_style: "primary"
      disable_button: false

oncall:
  initialized_only: true  # Initialize on-call but keep it disabled by default
  enable: false          # Not needed when initialized_only is true
  wait_minutes: 2  # Wait 2 minutes before escalating critical alerts
  provider: aws_incident_manager  # Specify AWS Incident Manager as the on-call provider

  # AWS Incident Manager response plan for critical alerts only
  aws_incident_manager:
    response_plan_arn: "arn:aws:ssm-incidents::123456789012:response-plan/PlatformCriticalPlan"
    # Optional: Configure multiple response plans for different environments or teams
    other_response_plan_arns:
      app: "arn:aws:ssm-incidents::123456789012:response-plan/AppCriticalPlan"

redis:  # Required for on-call functionality
  insecure_skip_verify: false  # production setting
  host: ${REDIS_HOST}
  port: ${REDIS_PORT}
  password: ${REDIS_PASSWORD}
  db: 0
```

This configuration allows Versus to:
- Use AWS response plans for critical alerts only
- Set a 2-minute wait time before escalation for critical alerts
- Ensure non-critical alerts don't trigger on-call processes

**Understanding the initialized_only Setting**

The `initialized_only: true` setting is a powerful feature that allows you to:

1. **Initialize the on-call system but keep it disabled by default**: The on-call infrastructure is set up and ready to use, but won't automatically trigger for any alerts.

2. **Enable on-call selectively using query parameters**: Only alerts that explicitly include `?oncall_enable=true` in their webhook URL will trigger the on-call workflow.

3. **Implement a critical-only approach**: Combined with Alert Manager routing rules, you can ensure only critical alerts with the right query parameters trigger on-call.

This approach provides several advantages:
- Greater control over which alerts can page on-call engineers
- Ability to test the on-call system without changing configuration
- Flexibility to adjust which services can trigger on-call without redeploying
- Protection against accidental on-call notifications during configuration changes

**Enhanced Slack Template**

Create an enhanced Slack template (`config/slack_message.tmpl`) that provides more context:

```
🔥 *{{ .commonLabels.severity | upper }} Alert: {{ .commonLabels.alertname }}*

🌐 *System*: `{{ .commonLabels.system }}`
🖥️ *Instance*: `{{ .commonLabels.instance }}`
🚨 *Status*: `{{ .status }}`
⏱️ *Detected*: `{{ .startsAt | date "Jan 02, 2006 15:04:05 MST" }}`

{{ range .alerts }}
📝 *Description*: {{ .annotations.description }}
{{ if .annotations.runbook }}📚 *Runbook*: {{ .annotations.runbook }}{{ end }}
{{ if .annotations.dashboard }}📊 *Dashboard*: {{ .annotations.dashboard }}{{ end }}
{{ end }}

{{ if .AckURL }}
⚠️ *Auto-escalation*: This alert will escalate {{ if eq .commonLabels.severity "critical" }}in 2 minutes{{ end }} if not acknowledged.
{{ end }}
```

This template provides additional context and clear timing expectations for responders.

### AWS IAM Role Configuration for Critical-Only Approach

For the critical-only on-call approach, you need to configure appropriate IAM permissions. This role allows Versus Incident to interact with AWS Incident Manager but only for critical incidents:

1. **Create a Dedicated IAM Policy**:

```json
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Action": [
                "ssm-incidents:StartIncident",
                "ssm-incidents:GetResponsePlan",
                "ssm-incidents:ListResponsePlans",
                "ssm-incidents:TagResource"
            ],
            "Resource": [
                "arn:aws:ssm-incidents:*:*:response-plan/*Critical*",
                "arn:aws:ssm-incidents:*:*:incident/*"
            ]
        },
        {
            "Effect": "Allow",
            "Action": [
                "ssm-contacts:GetContact",
                "ssm-contacts:ListContacts"
            ],
            "Resource": "*"
        }
    ]
}
```

This policy:
- Restricts incident creation to response plans containing "Critical" in the name
- Provides access to contacts for notification purposes
- Allows tagging of incidents for better organization

2. **Create an IAM Role**:
   - Create a new role with the appropriate trust relationship for your deployment environment (EC2, ECS, or Lambda)
   - Attach the policy created above
   - Note the Role ARN (e.g., `arn:aws:iam::111122223333:role/VersusIncidentCriticalRole`)

3. **Configure AWS Credentials**:
   - If using environment variables, set `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` from a user with the ability to assume this role
   - Alternatively, use EC2 instance profiles or AWS service roles for containerized deployments

This IAM configuration ensures that even if a non-critical incident tries to invoke the response plan, it will fail due to IAM permissions, providing an additional layer of enforcement for your critical-only on-call policy.

### Advanced Incident Routing Rules

Configure Alert Manager with advanced routing based on services, teams, and severity to work with the `initialized_only` setting:

```yaml
receivers:
- name: 'versus-normal'
  webhook_configs:
  - url: 'http://versus-service:3000/api/incidents'
    send_resolved: true

- name: 'versus-critical'
  webhook_configs:
  - url: 'http://versus-service:3000/api/incidents?oncall_enable=true'
    send_resolved: true

- name: 'versus-app-normal'
  webhook_configs:
  - url: 'http://versus-service:3000/api/incidents'
    send_resolved: true

- name: 'versus-app-critical'
  webhook_configs:
  - url: 'http://versus-service:3000/api/incidents?oncall_enable=true&awsim_other_response_plan=app'
    send_resolved: true

- name: 'versus-business-hours'
  webhook_configs:
  - url: 'http://versus-service:3000/api/incidents'
    send_resolved: true

route:
  receiver: 'versus-normal'  # Default receiver
  group_by: ['alertname', 'service', 'severity']
  
  # Time-based routing
  routes:
  - match_re:
      timeperiod: "business-hours"
    receiver: 'versus-business-hours'
  
  # Team and severity based routing - on-call only for critical
  - match:
      team: platform
      severity: critical
    receiver: 'versus-critical'
    
  - match:
      team: platform
      severity: high
    receiver: 'versus-normal'
    
  - match:
      team: application
      severity: critical
    receiver: 'versus-app-critical'
    
  - match:
      team: application
      severity: high
    receiver: 'versus-app-normal'
```

This configuration ensures that:
- On-call is completely disabled by default (even for critical alerts)
- Only alerts explicitly configured to trigger on-call will do so
- You have granular control over which alerts and severity levels can page your team
- You can easily test alert routing without risk of accidental paging

### Dynamic Configuration with Query Parameters

Versus Incident supports dynamic configuration through query parameters, which is especially powerful for managing on-call behavior when using `initialized_only: true`. These parameters can be added to your Alert Manager webhook URLs to override default settings on a per-alert basis:

| Query Parameter | Description | Example |
|-----------------|-------------|---------|
| `oncall_enable` | Enable or disable on-call for a specific alert | `?oncall_enable=true` |
| `oncall_wait_minutes` | Override the default wait time before escalation | `?oncall_wait_minutes=5` |
| `awsim_other_response_plan` | Use an alternative response plan defined in your configuration | `?awsim_other_response_plan=app` |

**Example Alert Manager Configurations**:

```yaml
# Immediately trigger on-call for database failures (no wait)
- name: 'versus-db-critical'
  webhook_configs:
  - url: 'http://versus-service:3000/api/incidents?oncall_enable=true&oncall_wait_minutes=0'
    send_resolved: true

# Use a custom response plan for network issues
- name: 'versus-network-critical'
  webhook_configs:
  - url: 'http://versus-service:3000/api/incidents?oncall_enable=true&awsim_other_response_plan=network'
    send_resolved: true
```

This flexibility allows you to fine-tune your incident response workflow based on the specific needs of different services and alert types while maintaining the critical-only approach to on-call escalation.

### Monitoring and Analytics

Implement metrics and reporting for your incident response process:

1. **Create CloudWatch Dashboards**:
   - Track incident frequency by service
   - Monitor Mean Time to Acknowledge (MTTA)
   - Monitor Mean Time to Resolve (MTTR)
   - Track escalation frequency
   - Visualize on-call burden distribution

2. **Set Up Regular Reporting**:
   - Configure automatic weekly reports of on-call activity
   - Track key metrics over time:
     + Number of incidents by severity
     + Acknowledge time by team, rotation, and individual
     + Resolution time
     + False positive rate

3. **Implement Continuous Improvement**:
   - Review metrics regularly with teams
   - Identify top sources of incidents
   - Track improvement initiatives
   - Use AWS Incident Manager's post-incident analysis feature

These analytics help identify patterns, reduce false positives, and enable teams to address systemic issues.

### Testing and Validation

Thoroughly test your advanced on-call workflows:

1. **Schedule Test Scenarios**:
   - During handoff periods between rotations
   - At different times of day
   - With different alert severities
   - During planned override periods

2. **Document Results**:
   - Track actual response times
   - Identify any notification failures
   - Ensure ChatBot integration works correctly
   - Validate metrics collection

3. **Conduct Regular Fire Drills**:
   - Schedule monthly unannounced test incidents
   - Rotate scenarios to test different aspects of the system
   - Include post-drill reviews and improvement plans

### Testing the Critical-Only Approach

You need to verify both that on-call is triggered with the right parameters and that it doesn't trigger by default:

1. **Test Default Behavior (Should NOT Trigger On-Call)**:
   
   ```bash
   # Send a critical alert WITHOUT oncall_enable parameter - should NOT trigger on-call
   curl -X POST "http://versus-service:3000/api/incidents" \
     -H "Content-Type: application/json" \
     -d '{
       "Logs": "[CRITICAL] This is a critical alert that should not trigger on-call",
       "ServiceName": "test-service",
       "Severity": "critical"
     }'
   ```

   Verify that:
   - The alert appears in your notification channels (Slack, etc.)
   - No AWS Incident Manager incident is created
   - No on-call team is notified

2. **Test Explicit On-Call Activation**:

   ```bash
   # Send a critical alert WITH oncall_enable=true - should trigger on-call after wait period
   curl -X POST "http://versus-service:3000/api/incidents?oncall_enable=true" \
     -H "Content-Type: application/json" \
     -d '{
       "Logs": "[CRITICAL] This is a critical alert that SHOULD trigger on-call",
       "ServiceName": "test-service",
       "Severity": "critical"
     }'
   ```

   Verify that:
   - The alert appears in your notification channels with an acknowledgment button
   - If not acknowledged within the wait period, an AWS Incident Manager incident is created
   - The appropriate on-call team is notified

3. **Test Immediate On-Call Activation**:

   ```bash
   # Send a critical alert with immediate on-call activation
   curl -X POST "http://versus-service:3000/api/incidents?oncall_enable=true&oncall_wait_minutes=0" \
     -H "Content-Type: application/json" \
     -d '{
       "Logs": "[CRITICAL] This is a critical alert that should trigger on-call IMMEDIATELY",
       "ServiceName": "test-service",
       "Severity": "critical"
     }'
   ```

   Verify that:
   - An AWS Incident Manager incident is created immediately
   - The on-call team is notified without waiting for acknowledgment

4. **Test Response Plan Override**:

   ```bash
   # Use a specific response plan
   curl -X POST "http://versus-service:3000/api/incidents?oncall_enable=true&awsim_other_response_plan=platform" \
     -H "Content-Type: application/json" \
     -d '{
       "Logs": "[CRITICAL] Platform issue requiring specific team",
       "ServiceName": "platform-service",
       "Severity": "critical"
     }'
   ```

   Verify that:
   - The correct response plan is used (check in AWS Incident Manager)
   - The appropriate platform team is engaged

### Conclusion

By implementing this advanced on-call management system with AWS Incident Manager and Versus Incident, you've created a advanced incident response workflow that:

- Automatically rotates on-call responsibilities among team members
- **Only triggers on-call for critical alerts with explicit activation**, preventing alert fatigue
- Routes incidents to the appropriate teams based on service and time
- Escalates critical incidents according to well-defined patterns
- Facilitates real-time collaboration during incidents
- Provides analytics for continuous improvement

This system ensures that critical incidents receive appropriate attention without unnecessary escalation for routine issues. For non-critical alerts, they're still visible in notification channels, but don't trigger the on-call escalation process.

Regularly review and refine your configurations as your organization and systems evolve. Solicit feedback from on-call engineers to identify pain points and improvement opportunities. Consider gathering metrics on the effectiveness of your approach, adjusting severity thresholds and query parameters as needed.

If you encounter any challenges or have questions about advanced configurations, refer to the AWS Incident Manager documentation or reach out to the Versus Incident community for support.