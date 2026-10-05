# Bento Amazon SES Plugin

A custom [Bento](https://github.com/warpstreamlabs/bento) plugin adding an
`aws_ses` output that sends emails through [Amazon SES](https://aws.amazon.com/ses/)
using the [SES v2 SendEmail API](https://docs.aws.amazon.com/ses/latest/APIReference-V2/API_SendEmail.html)
(`github.com/aws/aws-sdk-go-v2/service/sesv2`).

## Features

* **Plain text and HTML emails** — provide `text`, `html`, or both (sent as
  `multipart/alternative`).
* **Attachments** — attach one or more files per email.
* **Inline images** — embed images using a `Content-ID` and reference them from
  the HTML body with `cid:`.
* **Go templates everywhere** — `from`, `to`, `cc`, `bcc`, `reply_to`,
  `subject`, `text`, `html` and the attachment/image fields are all rendered as
  [Go templates](https://pkg.go.dev/text/template) against the message payload.
* **Familiar AWS credentials** — the same `credentials` configuration block used
  by every built-in Bento AWS component (shared file, static keys, profiles,
  assumed roles, EC2 role, expiry window).
* **Raw MIME** — messages are assembled as raw MIME so HTML, inline images and
  attachments all work together.

## Building and Running

Because this is a custom Bento plugin it must be compiled into a custom Bento
binary:

```bash
go build -o bento-ses .
./bento-ses -c workflow.yaml
```

## Configuration

```yaml
output:
  aws_ses:
    # Sender (required, Go template). Must be a verified identity in SES.
    from: "Alerts <alerts@example.com>"

    # Recipients (required, list of Go templates).
    to:
      - "{{ .email }}"
    cc: []
    bcc: []
    reply_to: []

    # Subject and bodies (Go templates). At least one body is required.
    subject: "Report for {{ .site }}"
    text: "Plain text body for {{ .site }}"
    html: "<h1>Hello {{ .site }}</h1>"

    # Optional SES configuration set.
    configuration_set: ""

    # Optional inline images (referenced from HTML as cid:<content_id>).
    images:
      - content_id: logo
        filename: logo.png
        content_type: image/png
        # Read from a file (path is a template) ...
        path: "/assets/logo.png"
        # ... or provide base64 inline (data is also a template).
        # data: "{{ base64 .logo }}"

    # Optional attachments.
    attachments:
      - filename: "{{ .report.filename }}"
        content_type: application/pdf
        # path: "/tmp/report.pdf"
        data: "{{ .report.content }}"

    timeout: 10s
    max_in_flight: 64

    # The standard Bento AWS session fields are available here.
    region: ""
    endpoint: ""
    credentials:
      profile: ""
      id: ""
      secret: ""
      token: ""
      from_ec2_role: false
      role: ""
      role_external_id: ""
      expiry_window: ""
```

### Template data context

The message payload is used as the template data. When the payload is a JSON
object its fields are available directly:

```json
{"recipient": "bob@example.com", "site": "example.org"}
```

```yaml
to: ["{{ .recipient }}"]
subject: "Report for {{ .site }}"
```

If the payload is not a JSON object it is exposed as `{{ .value }}` (and, for
non-JSON payloads, `{{ .raw }}`). Missing object keys render as empty strings.

The following helper functions are available in addition to the
[built-in template functions](https://pkg.go.dev/text/template#hdr-Functions):
`toJson`, `base64`, `upper`, `lower`, `trim`, `join`, `replace`, `default`,
`now` and `formatTime`.

### Images

Inline images are embedded using a `Content-ID` and referenced from the HTML
body with `cid:`:

```yaml
output:
  aws_ses:
    from: alerts@example.com
    to: ["bob@example.com"]
    subject: "Chart"
    html: '<h1>Latest chart</h1><img src="cid:chart">'
    images:
      - content_id: chart
        filename: chart.png
        content_type: image/png
        data: "{{ base64 .chart_png }}"
```

`path` and `data` are mutually exclusive; when `content_type` is omitted it is
inferred from the filename.

### Attachments

```yaml
output:
  aws_ses:
    from: reports@example.com
    to: ["{{ .recipient }}"]
    subject: "Daily report"
    text: "Please find the report attached."
    attachments:
      # From disk:
      - filename: report.pdf
        path: /var/reports/daily.pdf
      # From the message payload (base64):
      - filename: "{{ .name }}"
        content_type: application/octet-stream
        data: "{{ .content_b64 }}"
```

## Security

> **Security note:** when an attachment or image `path` is derived from
> untrusted message data, the component can be made to read arbitrary files
> from disk and email them. Prefer `data` (inline base64) or a constant/trusted
> path when the payload is not fully trusted.

## Credentials

The `credentials` block behaves exactly like the built-in Bento AWS components.
All of its fields are optional; with everything blank Bento falls back to the
shared credentials file (`~/.aws/credentials`, profile `default` unless
`AWS_PROFILE` is set).

Explicit static credentials:

```yaml
credentials:
  id: foo     # aws_access_key_id
  secret: bar # aws_secret_access_key
  token: baz  # aws_session_token (only for short term credentials)
```

Selecting a profile:

```yaml
credentials:
  profile: foo
```

Assuming a role (optionally with an external ID):

```yaml
credentials:
  role: arn:aws:iam::123456789012:role/my-role
  role_external_id: my-external-id
  expiry_window: 10s
```

## Message size

Amazon SES imposes a maximum message size (including attachments and base64
overhead). Keep this in mind when attaching files; an oversized message is
rejected by the API and the error is returned so Bento can retry or route it
according to your error handling configuration.

## Development

This project uses [Task](https://taskfile.dev) to manage common operations:

```bash
task build      # build ./bin/bento-ses
task test       # run unit tests
task test-e2e   # run end-to-end tests against the in-process kumo AWS emulator
task test-all   # unit + e2e
task vet        # go vet
task fmt        # gofmt
task tidy       # go mod tidy
task lint       # lint the example configs with the built binary
task run        # run the binary, e.g. task run -- -c workflow.yaml
task check      # vet + unit + e2e + config lint (pre-commit gate)
task clean      # remove build artifacts
```

The end-to-end tests are gated behind the `e2e` build tag and run against an
in-process [kumo](https://github.com/sivchari/kumo) AWS emulator, exercising the
real AWS SDK v2 SES client (config parsing, credentials handling, template
rendering, raw MIME assembly and the `SendEmail` call). The sent message is then
retrieved from kumo's inspection endpoint and its MIME structure is verified.
Run them directly with:

```bash
go test -tags e2e -count=1 -run EndToEnd -v ./ses/
```

> Kumo mounts SES v2 under the `/ses` path prefix. When pointing the component
> at kumo (or kumo's Docker image), set `endpoint` to `http://<host>:<port>/ses`.

## License

MIT
