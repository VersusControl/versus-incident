## How to Configure Sentry to Send Alerts to MS Teams

This guide will show you how to route Sentry alerts through Versus Incident to Microsoft Teams, enabling your team to respond to application issues quickly and efficiently.

**Prerequisites**
1. Microsoft Teams channel with webhook permissions
2. Sentry account with project owner permissions

### Set Up an MS Teams Webhook

First, create an incoming webhook in MS Teams to receive alerts from Versus Incident.

1. Open MS Teams and go to the channel where you want alerts to appear.
2. Click the three dots `(…)` next to the channel name and select Connectors.
3. Find Incoming Webhook, click Add, then Add again in the popup.
4. Name your webhook (e.g., Sentry Alerts) and optionally upload an image.
5. Click Create, then copy the generated webhook URL. Save this URL — you’ll need it later.

### Deploy Versus Incident with MS Teams Enabled

Next, configure Versus Incident to forward alerts to MS Teams using the webhook URL you created.

Create a directory for your configuration files:

```
mkdir -p ./config
```

Create `config/config.yaml` with the following content:

```yaml
name: versus
host: 0.0.0.0
port: 3000

alert:
  msteams:
    enable: true
    webhook_url: ${MSTEAMS_WEBHOOK_URL}
    template_path: "/app/config/msteams_message.tmpl"
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

`config/msteams_message.tmpl:`

```
**🚨 Sentry Alert: {{.data.issue.title}}**

**Project**: {{.data.issue.project.name}}

**Issue URL**: {{.data.issue.url}}

Please investigate this issue immediately.
```

This template uses Markdown to format the alert in MS Teams. It pulls data from the Sentry webhook payload (e.g., `{{.data.issue.title}}`).

Run Versus Incident using Docker, mounting your configuration files and setting the MS Teams webhook URL as an environment variable:

```bash
docker run -d \
  -p 3000:3000 \
  -v $(pwd)/config:/app/config \
  -e MSTEAMS_ENABLE=true \
  -e MSTEAMS_WEBHOOK_URL="your_teams_webhook_url" \
  --name versus \
  ghcr.io/versuscontrol/versus-incident
```

Replace `your_teams_webhook_url` with the webhook URL. The Versus Incident API endpoint for receiving alerts is now available at:

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