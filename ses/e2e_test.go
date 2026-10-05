//go:build e2e

// Package ses end-to-end tests are gated behind the "e2e" build tag so they
// can be run separately: `go test -tags e2e ./ses/`. They stand up an in-process
// kumo AWS emulator and exercise the real AWS SDK v2 SES client.

package ses

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sivchari/kumo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/warpstreamlabs/bento/public/service"
)

// sentEmail mirrors the subset of kumo's SentEmail payload used by the test.
type sentEmail struct {
	MessageID        string `json:"MessageId"`
	FromEmailAddress string `json:"FromEmailAddress"`
	Destination      *struct {
		ToAddresses  []string `json:"ToAddresses"`
		CcAddresses  []string `json:"CcAddresses"`
		BccAddresses []string `json:"BccAddresses"`
	} `json:"Destination"`
	Subject              string    `json:"Subject"`
	Body                 string    `json:"Body"`
	HTMLBody             string    `json:"HTMLBody"`
	RawData              []byte    `json:"RawData"`
	ConfigurationSetName string    `json:"ConfigurationSetName"`
	SentAt               time.Time `json:"SentAt"`
}

type sentEmailsResponse struct {
	SentEmails []sentEmail `json:"SentEmails"`
}

func fetchSentEmails(t *testing.T, baseURL string) []sentEmail {
	t.Helper()

	resp, err := http.Get(baseURL + "/kumo/ses/v2/sent-emails")
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var out sentEmailsResponse
	require.NoError(t, json.Unmarshal(body, &out))
	return out.SentEmails
}

// findSentEmail returns the single sent email matching subject. Kumo's storage
// is a process-wide singleton shared by every server instance, so tests filter
// by subject rather than assuming a clean slate.
func findSentEmail(t *testing.T, baseURL, subject string) sentEmail {
	t.Helper()

	var matches []sentEmail
	for _, e := range fetchSentEmails(t, baseURL) {
		if e.Subject == subject {
			matches = append(matches, e)
		}
	}
	require.Len(t, matches, 1, "expected exactly one sent email with subject %q", subject)
	return matches[0]
}

// TestSESOutputEndToEndWithKumo exercises the full path through the real AWS
// SDK v2 SES client against an in-process kumo emulator: config parsing, the
// credentials/session handling, template rendering, raw MIME assembly and the
// actual SendEmail API call. The sent message is then retrieved from kumo's
// inspection endpoint and its MIME structure verified.
func TestSESOutputEndToEndWithKumo(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	srv := kumo.NewServer()
	defer srv.Close()

	// Kumo mounts SES v2 under the "/ses" path prefix, so the base endpoint
	// must include it.
	endpoint := srv.URL + "/ses"

	spec := sesOutputSpec()
	pConf, err := spec.ParseYAML(fmt.Sprintf(`
region: us-east-1
endpoint: %q
credentials:
  id: test-id
  secret: test-secret
from: "Alerts <alerts@example.com>"
to:
  - "{{ .recipient }}"
cc:
  - "cc@example.com"
reply_to:
  - "no-reply@example.com"
subject: "Report for {{ .site }}"
text: "Plain body for {{ .site }}"
html: '<h1>Hello {{ .site }}</h1><img src="cid:logo">'
configuration_set: my-config-set
timeout: 10s
images:
  - content_id: logo
    filename: logo.png
    data: "{{ base64 .logo }}"
attachments:
  - filename: "{{ .report.filename }}"
    content_type: application/pdf
    path: "{{ .report.path }}"
`, endpoint), nil)
	require.NoError(t, err)

	w, err := newSESWriter(pConf, service.MockResources())
	require.NoError(t, err)

	w.readFile = func(path string) ([]byte, error) {
		if path == "/tmp/e2e-report.pdf" {
			return []byte("PDF-DATA"), nil
		}
		return nil, fmt.Errorf("unexpected path: %s", path)
	}

	ctx := context.Background()
	require.NoError(t, w.Connect(ctx))

	payload := `{
		"recipient": "bob@example.com",
		"site": "example.org",
		"logo": "PNGDATA",
		"report": {"filename": "daily.pdf", "path": "/tmp/e2e-report.pdf"}
	}`
	require.NoError(t, w.Write(ctx, service.NewMessage([]byte(payload))))
	require.NoError(t, w.Close(ctx))

	email := findSentEmail(t, srv.URL, "Report for example.org")

	assert.NotEmpty(t, email.MessageID)
	envelopeFrom, err := mail.ParseAddress(email.FromEmailAddress)
	require.NoError(t, err)
	assert.Equal(t, "alerts@example.com", envelopeFrom.Address)
	assert.Equal(t, "my-config-set", email.ConfigurationSetName)
	require.NotNil(t, email.Destination)
	assert.Equal(t, []string{"bob@example.com"}, email.Destination.ToAddresses)
	assert.Equal(t, []string{"cc@example.com"}, email.Destination.CcAddresses)
	require.NotEmpty(t, email.RawData)

	// Verify the raw MIME structure survived the round trip.
	msg, err := mail.ReadMessage(bytes.NewReader(email.RawData))
	require.NoError(t, err)
	assert.Equal(t, "Report for example.org", msg.Header.Get("Subject"))

	fromAddr, err := mail.ParseAddress(msg.Header.Get("From"))
	require.NoError(t, err)
	assert.Equal(t, "alerts@example.com", fromAddr.Address)
	assert.Equal(t, "Alerts", fromAddr.Name)

	var parts []*partInfo
	collectParts(t, textproto.MIMEHeader(msg.Header), msg.Body, &parts)

	byType := map[string]*partInfo{}
	for _, p := range parts {
		byType[p.contentType] = p
	}

	require.Contains(t, byType, "text/plain")
	require.Contains(t, byType, "text/html")
	require.Contains(t, byType, "image/png")
	require.Contains(t, byType, "application/pdf")

	assert.Equal(t, "Plain body for example.org", decodePart(t, byType["text/plain"]))
	assert.Contains(t, decodePart(t, byType["text/html"]), "cid:logo")
	assert.Equal(t, "PNGDATA", decodePart(t, byType["image/png"]))
	assert.Equal(t, "PDF-DATA", decodePart(t, byType["application/pdf"]))
	assert.Contains(t, byType["application/pdf"].disposition, "daily.pdf")
}

// TestSESOutputEndToEndTextOnlyWithKumo covers the simpler single-part path
// against kumo.
func TestSESOutputEndToEndTextOnlyWithKumo(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	srv := kumo.NewServer()
	defer srv.Close()

	spec := sesOutputSpec()
	pConf, err := spec.ParseYAML(fmt.Sprintf(`
region: us-east-1
endpoint: %q
credentials:
  id: test-id
  secret: test-secret
from: "alerts@example.com"
to: ["bob@example.com"]
subject: "Hello {{ .name }}"
text: "Hi {{ .name }}"
`, srv.URL+"/ses"), nil)
	require.NoError(t, err)

	w, err := newSESWriter(pConf, service.MockResources())
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, w.Connect(ctx))
	require.NoError(t, w.Write(ctx, service.NewMessage([]byte(`{"name":"Bob"}`))))
	require.NoError(t, w.Close(ctx))

	sent := findSentEmail(t, srv.URL, "Hello Bob")
	assert.Equal(t, "Hello Bob", sent.Subject)
	envelopeFrom, err := mail.ParseAddress(sent.FromEmailAddress)
	require.NoError(t, err)
	assert.Equal(t, "alerts@example.com", envelopeFrom.Address)
}

// TestSESOutputEndToEndImageAttachmentWithKumo attaches an image as a regular
// attachment and verifies both that kumo received the message and that the
// image bytes survived the full round trip.
func TestSESOutputEndToEndImageAttachmentWithKumo(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	srv := kumo.NewServer()
	defer srv.Close()

	pngBytes := singlePixelPNG(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "chart.png")
	require.NoError(t, os.WriteFile(path, pngBytes, 0o644))

	spec := sesOutputSpec()
	pConf, err := spec.ParseYAML(fmt.Sprintf(`
region: us-east-1
endpoint: %q
credentials:
  id: test-id
  secret: test-secret
from: "alerts@example.com"
to: ["{{ .recipient }}"]
subject: "Chart {{ .chart }}"
text: "See attached chart"
attachments:
  - filename: "{{ .filename }}"
    path: "{{ .path }}"
`, srv.URL+"/ses"), nil)
	require.NoError(t, err)

	w, err := newSESWriter(pConf, service.MockResources())
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, w.Connect(ctx))
	require.NoError(t, w.Write(ctx, service.NewMessage([]byte(fmt.Sprintf(
		`{"recipient":"bob@example.com","chart":"q1","filename":"q1-chart.png","path":%q}`, path,
	)))))
	require.NoError(t, w.Close(ctx))

	email := findSentEmail(t, srv.URL, "Chart q1")
	msg, err := mail.ReadMessage(bytes.NewReader(email.RawData))
	require.NoError(t, err)

	var parts []*partInfo
	collectParts(t, textproto.MIMEHeader(msg.Header), msg.Body, &parts)

	var imagePart *partInfo
	for _, p := range parts {
		if p.contentType == "image/png" {
			imagePart = p
		}
	}
	require.NotNil(t, imagePart, "expected an image/png part")
	assert.Contains(t, imagePart.disposition, "attachment")
	assert.Contains(t, imagePart.disposition, "q1-chart.png")

	got, err := decodeBase64Loose(string(imagePart.data))
	require.NoError(t, err)
	assert.Equal(t, pngBytes, got)
}
