# Migrating to v1.2.0

This guide provides instructions for migrating from v1.1.5 to v1.2.0.

## What's New in v1.2.0

Version 1.2.0 introduces enhanced Microsoft Teams integration using Power Automate, allowing you to send incident alerts directly to Microsoft Teams channels with more formatting options and better delivery reliability.

## Key Changes

The main change in v1.2.0 is the Microsoft Teams integration architecture:

* **Legacy webhook URLs replaced with Power Automate**: Instead of using the legacy Office 365 webhook URLs, Versus Incident now integrates with Microsoft Teams through Power Automate HTTP triggers, which provide more flexibility and reliability.

* **Configuration property names updated**: 
  - `webhook_url` → `power_automate_url`
  - `other_webhook_url` → `other_power_urls`

* **Environment variable names updated**:
  - `MSTEAMS_WEBHOOK_URL` → `MSTEAMS_POWER_AUTOMATE_URL`
  - `MSTEAMS_OTHER_WEBHOOK_URL_*` → `MSTEAMS_OTHER_POWER_URL_*`

* **API query parameter updated**:
  - `msteams_other_webhook_url` → `msteams_other_power_url`

## Configuration Changes

Here's a side-by-side comparison of the Microsoft Teams configuration in v1.1.5 vs v1.2.0:

### v1.1.5 (Before)

```yaml
alert:
  # ... other alert configurations ...
  
  msteams:
    enable: false # Default value, will be overridden by MSTEAMS_ENABLE env var
    webhook_url: ${MSTEAMS_WEBHOOK_URL}
    template_path: "config/msteams_message.tmpl"
    other_webhook_url: # Optional: Define additional webhook URLs
      qc: ${MSTEAMS_OTHER_WEBHOOK_URL_QC}
      ops: ${MSTEAMS_OTHER_WEBHOOK_URL_OPS}
      dev: ${MSTEAMS_OTHER_WEBHOOK_URL_DEV}
```

### v1.2.0 (After)

```yaml
alert:
  # ... other alert configurations ...
  
  msteams:
    enable: false # Default value, will be overridden by MSTEAMS_ENABLE env var
    power_automate_url: ${MSTEAMS_POWER_AUTOMATE_URL} # Power Automate HTTP trigger URL
    template_path: "config/msteams_message.tmpl"
    other_power_urls: # Optional: Enable overriding the default Power Automate flow
      qc: ${MSTEAMS_OTHER_POWER_URL_QC}
      ops: ${MSTEAMS_OTHER_POWER_URL_OPS}
      dev: ${MSTEAMS_OTHER_POWER_URL_DEV}
```

## Migration Steps

### 1. Update Your Configuration File

Replace the Microsoft Teams section in your `config.yaml` file:

```yaml
msteams:
  enable: false # Set to true to enable, or use MSTEAMS_ENABLE env var
  power_automate_url: ${MSTEAMS_POWER_AUTOMATE_URL} # Power Automate HTTP trigger URL
  template_path: "config/msteams_message.tmpl"
  other_power_urls: # Optional: Enable overriding the default Power Automate flow
    qc: ${MSTEAMS_OTHER_POWER_URL_QC}
    ops: ${MSTEAMS_OTHER_POWER_URL_OPS}
    dev: ${MSTEAMS_OTHER_POWER_URL_DEV}
```

### 2. Update Your Environment Variables

If you're using environment variables, update them:

```bash
# Old (v1.1.5)
MSTEAMS_WEBHOOK_URL=https://...
MSTEAMS_OTHER_WEBHOOK_URL_QC=https://...

# New (v1.2.0)
MSTEAMS_POWER_AUTOMATE_URL=https://...
MSTEAMS_OTHER_POWER_URL_QC=https://...
```

### 3. Setting up Power Automate for Microsoft Teams

To set up Microsoft Teams integration with Power Automate:

1. **Create a new Power Automate flow**:
   - Sign in to [Power Automate](https://flow.microsoft.com)
   - Click on "Create" → "Instant cloud flow"
   - Select "When a HTTP request is received" as the trigger

2. **Configure the HTTP trigger**:
   - The HTTP POST URL will be generated automatically after you save the flow
   - For the Request Body JSON Schema, you can use:

   ```json
   {
     "type": "object",
     "properties": {
       "message": {
         "type": "string"
       }
     }
   }
   ```

3. **Add a "Post message in a chat or channel" action**:
   - Click "+ New step"
   - Search for "Teams" and select "Post message in a chat or channel"
   - Configure the Teams channel where you want to post messages
   - In the Message field, use:
   ```
   @{triggerBody()?['message']}
   ```

4. **Save your flow and copy the HTTP POST URL**:
   - After saving, go back to the HTTP trigger step to see the generated URL
   - Copy this URL and use it for your `MSTEAMS_POWER_AUTOMATE_URL` environment variable or directly in your configuration file

### 4. Update Your API Calls

If you're making direct API calls that use the Teams integration, update your query parameters:

**Old (v1.1.5)**:
```
POST /api/incidents?msteams_other_webhook_url=qc
```

**New (v1.2.0)**:
```
POST /api/incidents?msteams_other_power_url=qc
```

### 5. Update Your Microsoft Teams Templates (Optional)

The template syntax remains the same, but you might want to review your templates to ensure they work correctly with the new integration. Here's a sample template for reference:

```
# Critical Error in {{.ServiceName}}
 
**Error Details:**

```{{.Logs}}```

Please investigate immediately
```

## Testing the Migration

After updating your configuration, test the Microsoft Teams integration to ensure it's working correctly:

```bash
curl -X POST http://your-versus-incident-server:3000/api/incidents \
  -H "Content-Type: application/json" \
  -d '{"service_name": "Test Service", "logs": "This is a test incident alert for Microsoft Teams integration"}'
```

## Additional Notes

- The older Microsoft Teams integration using webhook URLs still work after upgrading to v1.2.0, just update properties `webhook_url` → `power_automate_url`
- If you experience any issues with message delivery to Microsoft Teams, check your Power Automate flow run history to debug potential issues
- For organizations with multiple teams or departments, consider setting up separate Power Automate flows for each team and configuring them with the `other_power_urls` property