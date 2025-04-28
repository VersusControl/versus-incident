## How to Configure Sentry to Send Alerts to MS Teams

This guide will show you how to route Sentry alerts through Versus Incident to Microsoft Teams, enabling your team to respond to application issues quickly and efficiently.

**Prerequisites**
1. Microsoft Teams channel with Power Automate or webhook permissions
2. Sentry account with project owner permissions

### Set Up Microsoft Teams Integration (2025 Update)

Microsoft has announced the retirement of Office 365 Connectors (including Incoming Webhooks) by the end of 2025. Versus Incident supports both the legacy webhook method and the new Power Automate Workflows method. We recommend using Power Automate Workflows for all new deployments.

#### Option 1: Set Up a Power Automate Workflow (Recommended)

Follow these steps to create a Power Automate workflow to receive alerts in Microsoft Teams:

1. Sign in to [Power Automate](https://flow.microsoft.com/)
2. Click **Create** and select **Instant cloud flow**
3. Name your flow (e.g., "Versus Incident Alerts")
4. Select **When a HTTP request is received** as the trigger and click **Create**
5. In the HTTP trigger, you'll see a generated HTTP POST URL. Copy this URL - you'll need it later
6. Click **+ New step** and search for "Teams"
7. Select **Post a message in a chat or channel** (under Microsoft Teams)
8. Configure the action:
   - Choose **Channel** as the Post as option
   - Select your **Team** and **Channel**
   - For the **Message** field, add:
   ```
   @{triggerBody()?['messageText']}
   ```
9. Click **Save** to save your flow

#### Option 2: Set Up an MS Teams Webhook (Legacy Method)

For backward compatibility, Versus still supports the traditional webhook method (being retired by end of 2025):

1. Open MS Teams and go to the channel where you want alerts to appear.
2. Click the three dots `(…)` next to the channel name and select Connectors.
3. Find Incoming Webhook, click Add, then Add again in the popup.
4. Name your webhook (e.g., Sentry Alerts) and optionally upload an image.
5. Click Create, then copy the generated webhook URL. Save this URL — you'll need it later.

### Deploy Versus Incident with MS Teams Enabled

Next, configure Versus Incident to forward alerts to MS Teams. Create a directory for your configuration files:

```
mkdir -p ./config
```

Create `config/config.yaml` with the following content for Power Automate (recommended):

```yaml
name: versus
host: 0.0.0.0
port: 3000

alert:
  debug_body: true

  msteams:
    enable: false # Default value, will be overridden by MSTEAMS_ENABLE env var
    power_automate_url: ${MSTEAMS_POWER_AUTOMATE_URL} # Power Automate HTTP trigger URL
    template_path: "config/msteams_message.tmpl"
```

Create a custom MS Teams template in `config/msteams_message.tmpl`, for example, the JSON Format for Sentry Webhooks Integration:

```json
{
  "action": "created",
  "data": {
    "issue": {
      "id": "123456",
      "title": "Example Issue",
      "culprit": "example_function in example_module",
      "shortId": "PROJECT-1",
      "project": {
        "id": "1",
        "name": "Example Project",
        "slug": "example-project"
      },
      "metadata": {
        "type": "ExampleError",
        "value": "This is an example error"
      },
      "status": "unresolved",
      "level": "error",
      "firstSeen": "2023-10-01T12:00:00Z",
      "lastSeen": "2023-10-01T12:05:00Z",
      "count": 5,
      "userCount": 3
    }
  },
  "installation": {
    "uuid": "installation-uuid"
  },
  "actor": {
    "type": "user",
    "id": "789",
    "name": "John Doe"
  }
}
```

Now, create a rich MS Teams template in `config/msteams_message.tmpl`:

```markdown
**🚨 Sentry Alert: {{.data.issue.title}}**

**Project**: {{.data.issue.project.name}}

**Issue URL**: {{.data.issue.url}}

Please investigate this issue immediately.
```

This template uses Markdown to format the alert in MS Teams. It pulls data from the Sentry webhook payload (e.g., `{{.data.issue.title}}`).

**Note about MS Teams notifications (April 2025)**: The system will automatically extract "Sentry Alert: {{.data.issue.title}}" as the summary for Microsoft Teams notifications, and generate a plain text version as a fallback. You don't need to add these fields manually - Versus Incident handles this to ensure proper display in Microsoft Teams.

Run Versus Incident using Docker, mounting your configuration files and setting the MS Teams Power Automate URL as an environment variable:

```bash
docker run -d \
  -p 3000:3000 \
  -v $(pwd)/config:/app/config \
  -e MSTEAMS_ENABLE=true \
  -e MSTEAMS_POWER_AUTOMATE_URL="your_power_automate_url" \
  --name versus \
  ghcr.io/versuscontrol/versus-incident
```

Replace `your_power_automate_url` with the URL you copied from Power Automate. The Versus Incident API endpoint for receiving alerts is now available at:

```
http://localhost:3000/api/incidents
```

### Configure Sentry Alerts with a Webhook

Now, set up Sentry to send alerts to Versus Incident via a webhook.

1. Log in to your Sentry account and navigate to your project.
2. Go to Alerts in the sidebar and click Create Alert Rule.
3. Define the conditions for your alert, such as:
+ When: “A new issue is created”
+ Filter: (Optional) Add filters like “error level is fatal”
4. Under Actions, select Send a notification via a webhook.
5. Enter the webhook URL:
+ If Versus is running locally: `http://localhost:3000/api/incidents`
+ If deployed elsewhere: `https://your-versus-domain.com/api/incidents`
6. Ensure the HTTP method is POST and the content type is application/json.
7. Save the alert rule.

Sentry will now send a JSON payload to Versus Incident whenever the alert conditions are met.

### Test the Integration

To confirm everything works, simulate a Sentry alert using curl:

```bash
curl -X POST http://localhost:3000/api/incidents \
  -H "Content-Type: application/json" \
  -d '{
    "action": "triggered",
    "data": {
      "issue": {
        "id": "123456",
        "title": "Test Error: Something went wrong",
        "shortId": "PROJECT-1",
        "project": {
          "name": "Test Project",
          "slug": "test-project"
        },
        "url": "https://sentry.io/organizations/test-org/issues/123456/"
      }
    }
  }'
```

Alternatively, trigger a real error in your Sentry-monitored application and verify the alert appears in MS Teams.

### Conclusion

By connecting Sentry to MS Teams via Versus Incident, you’ve created a streamlined alerting system that keeps your team informed of critical issues in real-time. Versus Incident’s flexibility allows you to tailor alerts to your needs and expand to other channels as required.