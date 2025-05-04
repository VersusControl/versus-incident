## Configure Kibana to Send Alerts to Slack and Telegram

## Table of Contents
- [Prerequisites](#prerequisites)
- [Step 1: Set Up Slack and Telegram Bots](#step-1-set-up-slack-and-telegram-bots)
  - [Slack Bot](#slack-bot)
  - [Telegram Bot](#telegram-bot)
- [Step 2: Deploy Versus Incident with Slack and Telegram Enabled](#step-2-deploy-versus-incident-with-slack-and-telegram-enabled)
  - [Create Configuration Files](#create-configuration-files)
  - [Run Versus Incident with Docker](#run-versus-incident-with-docker)
- [Step 3: Configure Kibana Alerts with a Webhook](#step-3-configure-kibana-alerts-with-a-webhook)
- [Step 4: Test the Integration](#step-4-test-the-integration)
- [Conclusion](#conclusion)

Kibana, part of the Elastic Stack, provides powerful monitoring and alerting capabilities for your applications and infrastructure. However, its native notification options are limited.

In this guide, we’ll walk through setting up Kibana to send alerts to Versus Incident, which will then forward them to Slack and Telegram using custom templates.

## Prerequisites

- A running Elastic Stack (Elasticsearch and Kibana) instance with alerting enabled (Kibana 7.13+ required for the Alerting feature).
- A Slack workspace with permissions to create a bot and obtain a token.
- A Telegram account with a bot created via BotFather and a chat ID for your target group or channel.
- Docker installed (optional, for easy Versus Incident deployment).

## Step 1: Set Up Slack and Telegram Bots

### Slack Bot
1. Visit [api.slack.com/apps](https://api.slack.com/apps) and click **Create New App**.
2. Name your app (e.g., “Kibana Alerts”) and select your Slack workspace.
3. Under **Bot Users**, add a bot (e.g., “KibanaBot”) and enable it.
4. Go to **OAuth & Permissions**, add the `chat:write` scope under **Scopes**.
5. Install the app to your workspace and copy the **Bot User OAuth Token** (starts with `xoxb-`). Save it securely.
6. Invite the bot to your Slack channel by typing `/invite @KibanaBot` in the channel and note the channel ID (right-click the channel, copy the link, and extract the ID).

### Telegram Bot
1. Open Telegram and search for **BotFather**.
2. Start a chat and type `/newbot`. Follow the prompts to name your bot (e.g., “KibanaAlertBot”).
3. BotFather will provide a **Bot Token** (e.g., `123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11`). Save it securely.
4. Create a group or channel in Telegram, add your bot, and get the **Chat ID**:
   - Send a message to the group/channel via the bot.
   - Use `https://api.telegram.org/bot<YourBotToken>/getUpdates` in a browser to retrieve the `chat.id` (e.g., `-123456789`).

## Step 2: Deploy Versus Incident with Slack and Telegram Enabled

Versus Incident acts as a bridge between Kibana and your notification channels. We’ll configure it to handle both Slack and Telegram alerts.

### Create Configuration Files

1. Create a directory for configuration:

```bash
mkdir -p ./config
```

2. Create `config/config.yaml` with the following content:

```yaml
name: versus
host: 0.0.0.0
port: 3000

alert:
  slack:
    enable: true
    token: ${SLACK_TOKEN}
    channel_id: ${SLACK_CHANNEL_ID}
    template_path: "/app/config/slack_message.tmpl"

  telegram:
    enable: true
    bot_token: ${TELEGRAM_BOT_TOKEN}
    chat_id: ${TELEGRAM_CHAT_ID}
    template_path: "/app/config/telegram_message.tmpl"
```

3. Create a Slack template at `config/slack_message.tmpl`:

```plaintext
🚨 *Kibana Alert: {{.name}}*

**Message**: {{.message}}
**Status**: {{.status}}
**Kibana URL**: <{{.kibanaUrl}}|View in Kibana>

Please investigate this issue.
```

4. Create a Telegram template at `config/telegram_message.tmpl` (using HTML formatting):

```plaintext
🚨 <b>Kibana Alert: {{.name}}</b>

<b>Message</b>: {{.message}}
<b>Status</b>: {{.status}}
<b>Kibana URL</b>: <a href="{{.kibanaUrl}}">View in Kibana</a>

Please investigate this issue.
```

### Run Versus Incident with Docker

Deploy Versus Incident with the configuration and environment variables:

```bash
docker run -d \
  -p 3000:3000 \
  -v $(pwd)/config:/app/config \
  -e SLACK_ENABLE=true \
  -e SLACK_TOKEN="your_slack_bot_token" \
  -e SLACK_CHANNEL_ID="your_slack_channel_id" \
  -e TELEGRAM_ENABLE=true \
  -e TELEGRAM_BOT_TOKEN="your_telegram_bot_token" \
  -e TELEGRAM_CHAT_ID="your_telegram_chat_id" \
  --name versus \
  ghcr.io/versuscontrol/versus-incident
```

- Replace `your_slack_bot_token` and `your_slack_channel_id` with Slack values.
- Replace `your_telegram_bot_token` and `your_telegram_chat_id` with Telegram values.

The Versus Incident API endpoint is now available at `http://localhost:3000/api/incidents`.

## Step 3: Configure Kibana Alerts with a Webhook

Kibana’s Alerting feature allows you to send notifications via webhooks. We’ll configure it to send alerts to Versus Incident.

1. Log in to Kibana and go to **Stack Management > Alerts and Insights > Rules**.
2. Click **Create Rule**.
3. Define your rule:
   - **Name**: e.g., “High CPU Alert”.
   - **Connector**: Select an index or data view to monitor (e.g., system metrics).
   - **Condition**: Set a condition, such as “CPU usage > 80% over the last 5 minutes”.
   - **Check every**: 1 minute (or your preferred interval).
4. Add an **Action**:
   - **Action Type**: Select **Webhook**.
   - **URL**: `http://localhost:3000/api/incidents` (or your deployed Versus URL, e.g., `https://your-versus-domain.com/api/incidents`).
   - **Method**: POST.
   - **Headers**: Add `Content-Type: application/json`.
   - **Body**: Use this JSON template to match Versus Incident’s expected fields:
     ```json
     {
       "name": "{{rule.name}}",
       "message": "{{context.message}}",
       "status": "{{alert.state}}",
       "kibanaUrl": "{{kibanaBaseUrl}}/app/management/insightsAndAlerting/rules/{{rule.id}}"
     }
     ```
5. Save the rule.

Kibana will now send a JSON payload to Versus Incident whenever the alert condition is met.

## Step 4: Test the Integration

Simulate a Kibana alert using `curl` to test the setup:

```bash
curl -X POST http://localhost:3000/api/incidents \
  -H "Content-Type: application/json" \
  -d '{
    "name": "High CPU Alert",
    "message": "CPU usage exceeded 80% on server-01",
    "status": "active",
    "kibanaUrl": "https://your-kibana-instance.com/app/management/insightsAndAlerting/rules/12345"
  }'
```

Alternatively, trigger a real alert in Kibana (e.g., by simulating high CPU usage in your monitored system) and confirm the notifications appear in both Slack and Telegram.

## Conclusion

By integrating Kibana with Versus Incident, you can send alerts to Slack and Telegram with customized, actionable messages that enhance your team’s incident response. This setup is flexible and scalable—Versus Incident also supports additional channels like Microsoft Teams and Email, as well as on-call integrations like AWS Incident Manager.

If you encounter any issues or have further questions, feel free to reach out!
