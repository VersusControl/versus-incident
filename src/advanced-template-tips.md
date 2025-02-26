## Advanced Template Tips

### Multi-Service Template

Handle multiple alerts in one template:

```
{{ $service := .source | replace "aws." "" | upper }}
📡 *{{$service}} Alert*

{{ if eq .source "aws.glue" }}
  🔧 Job: {{.detail.jobName}}
{{ else if eq .source "aws.ec2" }}
  🖥 Instance: {{.detail.instance-id}}
{{ end }}

🔗 *Details*: {{.detail | toJson}}
```

### Conditional Formatting

Highlight critical issues:

```
{{ if gt .detail.actualValue .detail.threshold }}
🚨 CRITICAL: {{.detail.alarmName}} ({{.detail.actualValue}}%)
{{ else }}
⚠️ WARNING: {{.detail.alarmName}} ({{.detail.actualValue}}%)
{{ end }}
```

### Best Practices for Custom Templates

1. Keep It Simple: Focus on the most critical details for each alert.
2. Use Conditional Logic: Tailor messages based on event severity or type.
3. Test Your Templates: Use sample SNS messages to validate your templates.
4. Document Your Templates: Share templates with your team for consistency.
