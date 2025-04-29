## Understanding PagerDuty On-Call

PagerDuty is a popular incident management platform that provides robust on-call scheduling, alerting, and escalation capabilities. This document explains the key components of PagerDuty's on-call system—services, escalation policies, schedules, and integrations—in a simple and clear way.

### Key Components of PagerDuty On-Call

PagerDuty's on-call system relies on four main components: services, escalation policies, schedules, and integrations. Let's explore each one in detail.

### 1. Services

Services in PagerDuty represent the applications, components, or systems that you monitor. Each service:

+ Has a unique name and description
+ Is associated with an escalation policy
+ Can be integrated with monitoring tools
+ Contains a set of alert/incident settings

When an incident is triggered, it's associated with a specific service, which determines how the incident is handled and who is notified.

**Example:** A "Payment Processing API" service might be set up to:
+ Alert the backend team when it experiences errors
+ Have high urgency for all incidents
+ Auto-resolve incidents after 24 hours if fixed

### 2. Escalation Policies

Escalation policies define who gets notified about an incident and in what order. They ensure that incidents are addressed even if the first responder isn't available.

An escalation policy typically includes:
+ One or more escalation levels with designated responders
+ Time delays between escalation levels
+ Options to repeat the escalation process if no one responds

**Example:** For the "Payment API" service, an escalation policy might:
+ Level 1: Notify the on-call engineer on the primary schedule
+ Level 2: If no response in 15 minutes, notify the secondary on-call engineer
+ Level 3: If still no response in 10 minutes, notify the engineering manager

### 3. Schedules

Schedules determine who is on-call at any given time. They allow teams to:
+ Define rotation patterns (daily, weekly, custom)
+ Set up multiple layers of coverage
+ Handle time zone differences
+ Plan for holidays and time off

PagerDuty's schedules are highly flexible and can accommodate complex team structures and rotation patterns.

**Example:** A "Backend Team Primary" schedule might rotate three engineers weekly, with handoffs occurring every Monday at 9 AM local time. A separate "Backend Team Secondary" schedule might rotate a different group of engineers as backup.

### 4. Integrations

Integrations connect PagerDuty to your monitoring tools, allowing alerts to be automatically converted into PagerDuty incidents. PagerDuty offers hundreds of integrations with popular monitoring systems.

For custom systems or tools without direct integrations, PagerDuty provides:
+ Events API (V2) - A simple API for sending alerts to PagerDuty
+ Webhooks - For receiving data about PagerDuty incidents in your other systems

**Example:** A company might integrate:
+ Prometheus Alert Manager with their "Infrastructure" service
+ Application error tracking with their "Application Errors" service
+ Custom business logic monitors with their "Business Metrics" service

### The PagerDuty Incident Lifecycle

When an incident is created in PagerDuty:

1. **Trigger**: An alert comes in from an integrated monitoring system or API call
2. **Notification**: PagerDuty notifies the appropriate on-call person based on the escalation policy
3. **Acknowledgment**: The responder acknowledges the incident, letting others know they're working on it
4. **Resolution**: After fixing the issue, the responder resolves the incident
5. **Post-Mortem**: Teams can analyze what happened and how to prevent similar issues

This structured approach ensures that incidents are handled efficiently and consistently.

### Key Benefits of PagerDuty for On-Call Management

+ **Reliability**: Ensures critical alerts never go unnoticed with multiple notification methods and escalation paths
+ **Flexibility**: Supports complex team structures and rotation patterns
+ **Reduced Alert Fatigue**: Intelligent grouping and routing of alerts to the right people
+ **Comprehensive Visibility**: Dashboards and reports to track incident metrics and on-call load
+ **Integration Ecosystem**: Works with virtually any monitoring or alerting system

Next, we will provide a step-by-step guide to integrating **Versus** with PagerDuty for On-Call: **[Integration](./how-to-integration-pagerduty.md)**.