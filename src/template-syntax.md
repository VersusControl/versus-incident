# Template Syntax Guide

This document explains the template syntax (Go template syntax) used for create a custom alert template in Versus Incident.

## Table of Contents
- [Basic Syntax](#basic-syntax)
  - [Access Data](#access-data)
  - [Variables](#variables)
  - [Pipelines](#pipelines)
- [Control Structures](#control-structures)
  - [Conditionals (if/else)](#conditionals)
  - [Loops (range)](#loops)
- [Microsoft Teams Templates](#microsoft-teams-templates)

## Basic Syntax

### Access Data

Access data fields using double curly braces and dot notation, for example, with the data:

```json
{
  "Logs": "[ERROR] This is an error log from User Service that we can obtain using Fluent Bit.",
  "ServiceName": "order-service",
}
```

Example template:
```
*Error in {{ .ServiceName }}*
{{ .Logs }}
```

### Variables
You can declare variables within a template using the {{ $variable := value }} syntax. Once declared, variables can be used throughout the template, for example:

```
{{ $owner := "Team Alpha" }}
Owner: {{ $owner }}
```

Output:
```
Owner: Team Alpha
```

### Pipelines
Pipelines allow you to chain together multiple actions or functions. The result of one action can be passed as input to another, for example:

**upper**: Converts a string to uppercase.

```
*{{ .ServiceName | upper }} Failure*
```

**lower**: Converts a string to lowercase.

```
*{{ .ServiceName | lower }} Failure*
```

**title**: Converts a string to title case (first letter of each word capitalized).

```
*{{ .ServiceName | title }} Failure*
```

**default**: Provides a default value if the input is empty.

```
*{{ .ServiceName | default "unknown-service" }} Failure*
```

**slice**: Extracts a sub-slice from a slice or string.

```
{{ .Logs | slice 0 50 }}  // First 50 characters
```

**replace**: Replaces occurrences of a substring.

```
{{ .Logs | replace "error" "issue" }}
```

**trimPrefix**: Trims a prefix from a string.

```
{{ .Logs | trimPrefix "prod-" }}
```

**trimSuffix**: Trims a suffix from a string.

```
{{ .Logs | trimSuffix "-service" }}
```

**len**: Returns the length

```
{{ .Logs | len }}  // Length of the message
```

**urlquery**: Escapes a string for use in a URL query.

```
uri /search?q={{ .Query | urlquery }}
```

**split**: splits a string into array using a separator.

```
{{ $parts := split "apple,banana,cherry" "," }}

{{/* Iterate over split results */}}
{{ range $parts }}
  {{ . }}
{{ end }}
```

**You can chain multiple pipes together**:

```
{{ .Logs | trim | lower | truncate 50 }}
```

## Control Structures

### Conditionals

The templates support conditional logic using if, else, and end keywords.

```
{{ if .IsCritical }}
🚨 CRITICAL ALERT 🚨
{{ else }}
⚠️ Warning Alert ⚠️
{{ end }}
```

and:

```
{{ and .Value1 .Value2 .Value3 }}
```

or:

```
{{ or .Value1 .Value2 "default" }}
```

**Best Practices**

Error Handling:

```
{{ If .Error }}
  {{ .Details }}
{{ else }}
  No error details
{{ end }}
```

Whitespace Control:

```
{{- if .Production }}  // Remove preceding whitespace
PROD ALERT{{ end -}}   // Remove trailing whitespace
```

Template Comments:

```
{{/* This is a hidden comment */}}
```

Negates a boolean value:

```
{{ if not .IsCritical }}
  This is not a critical issue.
{{ end }}
```

Checks if two values are equal:

```
{{ if eq .Status "critical" }}
  🚨 Critical Alert 🚨
{{ end }}
```

Checks if two values are not equal:

```
{{ if ne .Env "production" }}
  This is not a production environment.
{{ end }}
```

Returns the length of a string, slice, array, or map:

```
{{ if gt (len .Errors) 0 }}
  There are {{ len .Errors }} errors.
{{ end }}
```

Checks if a string has a specific prefix:

```
{{ if .ServiceName | hasPrefix "prod-" }}
  Production service!
{{ end }}
```

Checks if a string has a specific suffix:

```
{{ if .ServiceName | hasSuffix "-service" }}
  This is a service.
{{ end }}
```

Checks if a message contains a specific strings:

```
{{ if contains .Logs "error" }}
  The message contains error logs.
{{ else }}
  The message does NOT contain error.
{{ end }}
```

### Loops

Iterate over slices/arrays with range:

```
{{ range .ErrorStack }}
- {{ . }}
{{ end }}
```

## Microsoft Teams Templates

Microsoft Teams templates support Markdown syntax, which is automatically converted to Adaptive Cards when sent to Teams. As of April 2025 (with the retirement of Office 365 Connectors), all Microsoft Teams integrations use Power Automate Workflows.

### Supported Markdown Features

Your template can include:

- **Headings**: Use `#`, `##`, or `###` for different heading levels
- **Bold Text**: Wrap text with double asterisks (`**bold**`)
- **Code Blocks**: Use triple backticks to create code blocks
- **Lists**: Create unordered lists with `-` or `*`, and ordered lists with numbers
- **Links**: Use `[text](url)` to create clickable links

### Automatic Summary and Text Fields

Versus Incident now automatically handles two important fields for Microsoft Teams notifications:

1. **Summary**: The system extracts a summary from your template's first heading (or first line if no heading exists) which appears in Teams notifications.
2. **Text**: A plain text version of your message is automatically generated as a fallback for clients that don't support Adaptive Cards.

You don't need to add these fields manually - the system handles this for you to ensure proper display in Microsoft Teams.

### Example Template

Here's a complete example for Microsoft Teams:

```markdown
# Incident Alert: {{.ServiceName}}

### Error Information
**Time**: {{.Timestamp}}
**Severity**: {{.Severity}}

## Error Details
```{{.Logs}}```

## Action Required
1. Check system status
2. Review logs in monitoring dashboard
3. Escalate to on-call if needed

[View Details](https://your-dashboard/incidents/{{.IncidentID}})
```

This will be converted to an Adaptive Card with proper formatting in Microsoft Teams, with headings, code blocks, formatted lists, and clickable links.