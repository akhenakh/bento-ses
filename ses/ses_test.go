package ses

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/warpstreamlabs/bento/public/service"
)

type mockSESClient struct {
	lastInput *sesv2.SendEmailInput
	calls     int
	err       error
}

func (m *mockSESClient) SendEmail(_ context.Context, params *sesv2.SendEmailInput, _ ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
	m.calls++
	m.lastInput = params
	if m.err != nil {
		return nil, m.err
	}
	return &sesv2.SendEmailOutput{}, nil
}

type partInfo struct {
	contentType string
	disposition string
	contentID   string
	encoding    string
	data        []byte
}

func collectParts(t *testing.T, header textproto.MIMEHeader, body io.Reader, out *[]*partInfo) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	require.NoError(t, err)

	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			collectParts(t, textproto.MIMEHeader(p.Header), p, out)
		}
		return
	}

	data, err := io.ReadAll(body)
	require.NoError(t, err)
	*out = append(*out, &partInfo{
		contentType: mediaType,
		disposition: header.Get("Content-Disposition"),
		contentID:   header.Get("Content-ID"),
		encoding:    header.Get("Content-Transfer-Encoding"),
		data:        data,
	})
}

func TestSESOutputWrite(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	spec := sesOutputSpec()
	pConf, err := spec.ParseYAML(`
region: us-east-1
endpoint: ""
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
html: "<h1>Hello {{ .site }}</h1><img src=\"cid:logo\">"
configuration_set: my-config-set
timeout: 5s
images:
  - content_id: logo
    filename: logo.png
    data: "{{ base64 .logo }}"
attachments:
  - filename: "{{ .report.filename }}"
    content_type: application/pdf
    path: "{{ .report.path }}"
`, nil)
	require.NoError(t, err)

	mgr := service.MockResources()
	w, err := newSESWriter(pConf, mgr)
	require.NoError(t, err)

	mock := &mockSESClient{}
	w.client = mock

	// Resolve attachment paths through the in-memory fake.
	w.readFile = func(path string) ([]byte, error) {
		if path == "/tmp/report.pdf" {
			return []byte("PDF-DATA"), nil
		}
		return nil, fmt.Errorf("unexpected path: %s", path)
	}

	payload := `{
		"recipient": "bob@example.com",
		"site": "example.org",
		"logo": "PNGDATA",
		"report": {"filename": "daily.pdf", "path": "/tmp/report.pdf"}
	}`

	err = w.Write(context.Background(), service.NewMessage([]byte(payload)))
	require.NoError(t, err)
	require.Equal(t, 1, mock.calls)

	in := mock.lastInput
	require.NotNil(t, in)
	assert.Equal(t, "Alerts <alerts@example.com>", *in.FromEmailAddress)
	assert.Equal(t, []string{"bob@example.com"}, in.Destination.ToAddresses)
	assert.Equal(t, []string{"cc@example.com"}, in.Destination.CcAddresses)
	assert.Equal(t, []string{"no-reply@example.com"}, in.ReplyToAddresses)
	require.NotNil(t, in.ConfigurationSetName)
	assert.Equal(t, "my-config-set", *in.ConfigurationSetName)
	require.NotNil(t, in.Content.Raw)

	// Parse the generated raw message.
	msg, err := mail.ReadMessage(bytes.NewReader(in.Content.Raw.Data))
	require.NoError(t, err)
	assert.Equal(t, "Report for example.org", msg.Header.Get("Subject"))
	fromAddr, err := mail.ParseAddress(msg.Header.Get("From"))
	require.NoError(t, err)
	assert.Equal(t, "alerts@example.com", fromAddr.Address)
	assert.Equal(t, "Alerts", fromAddr.Name)

	toAddrs, err := mail.ParseAddressList(msg.Header.Get("To"))
	require.NoError(t, err)
	require.Len(t, toAddrs, 1)
	assert.Equal(t, "bob@example.com", toAddrs[0].Address)

	ccAddrs, err := mail.ParseAddressList(msg.Header.Get("Cc"))
	require.NoError(t, err)
	require.Len(t, ccAddrs, 1)
	assert.Equal(t, "cc@example.com", ccAddrs[0].Address)

	replyAddrs, err := mail.ParseAddressList(msg.Header.Get("Reply-To"))
	require.NoError(t, err)
	require.Len(t, replyAddrs, 1)
	assert.Equal(t, "no-reply@example.com", replyAddrs[0].Address)

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

	assert.Contains(t, decodePart(t, byType["text/html"]), "cid:logo")
	assert.Equal(t, "<logo>", byType["image/png"].contentID)
	assert.Contains(t, byType["image/png"].disposition, "inline")
	assert.Contains(t, byType["application/pdf"].disposition, "attachment")
	assert.Contains(t, byType["application/pdf"].disposition, "daily.pdf")
}

func TestSESOutputTextOnly(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	spec := sesOutputSpec()
	pConf, err := spec.ParseYAML(`
region: us-east-1
credentials:
  id: test-id
  secret: test-secret
from: "alerts@example.com"
to: ["bob@example.com"]
subject: "Hi"
text: "Hello {{ .name }}"
`, nil)
	require.NoError(t, err)

	w, err := newSESWriter(pConf, service.MockResources())
	require.NoError(t, err)
	mock := &mockSESClient{}
	w.client = mock

	require.NoError(t, w.Write(context.Background(), service.NewMessage([]byte(`{"name":"Bob"}`))))

	msg, err := mail.ReadMessage(bytes.NewReader(mock.lastInput.Content.Raw.Data))
	require.NoError(t, err)

	var parts []*partInfo
	collectParts(t, textproto.MIMEHeader(msg.Header), msg.Body, &parts)
	require.Len(t, parts, 1)
	assert.Equal(t, "text/plain", parts[0].contentType)

	decoded := decodePart(t, parts[0])
	assert.Equal(t, "Hello Bob", decoded)
}

func TestSESOutputAttachmentFromRealFile(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	require.NoError(t, os.WriteFile(path, []byte("file-content"), 0o644))

	spec := sesOutputSpec()
	pConf, err := spec.ParseYAML(fmt.Sprintf(`
region: us-east-1
credentials:
  id: test-id
  secret: test-secret
from: "alerts@example.com"
to: ["bob@example.com"]
text: "body"
attachments:
  - filename: hello.txt
    path: %q
`, path), nil)
	require.NoError(t, err)

	w, err := newSESWriter(pConf, service.MockResources())
	require.NoError(t, err)
	mock := &mockSESClient{}
	w.client = mock

	require.NoError(t, w.Write(context.Background(), service.NewMessage([]byte(`{}`))))

	msg, err := mail.ReadMessage(bytes.NewReader(mock.lastInput.Content.Raw.Data))
	require.NoError(t, err)

	var parts []*partInfo
	collectParts(t, textproto.MIMEHeader(msg.Header), msg.Body, &parts)
	require.Len(t, parts, 2)
	assert.Equal(t, "text/plain", parts[0].contentType)
	assert.Equal(t, "text/plain", parts[1].contentType)
	assert.Equal(t, "file-content", decodePart(t, parts[1]))
}

// singlePixelPNG returns a valid, tiny PNG used by the image attachment tests.
func singlePixelPNG(t *testing.T) []byte {
	t.Helper()
	b, err := decodeBase64Loose("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=")
	require.NoError(t, err)
	return b
}

// TestSESOutputImageAttachment attaches an image as a regular attachment (as
// opposed to an inline image) and checks that its content type is inferred from
// the filename and that its bytes survive intact.
func TestSESOutputImageAttachment(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	pngBytes := singlePixelPNG(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "chart.png")
	require.NoError(t, os.WriteFile(path, pngBytes, 0o644))

	spec := sesOutputSpec()
	pConf, err := spec.ParseYAML(fmt.Sprintf(`
region: us-east-1
credentials:
  id: test-id
  secret: test-secret
from: "alerts@example.com"
to: ["bob@example.com"]
text: "See attached chart"
attachments:
  - filename: q1-chart.png
    path: %q
`, path), nil)
	require.NoError(t, err)

	w, err := newSESWriter(pConf, service.MockResources())
	require.NoError(t, err)
	mock := &mockSESClient{}
	w.client = mock

	require.NoError(t, w.Write(context.Background(), service.NewMessage([]byte(`{}`))))

	msg, err := mail.ReadMessage(bytes.NewReader(mock.lastInput.Content.Raw.Data))
	require.NoError(t, err)

	var parts []*partInfo
	collectParts(t, textproto.MIMEHeader(msg.Header), msg.Body, &parts)
	require.Len(t, parts, 2)

	assert.Equal(t, "text/plain", parts[0].contentType)
	assert.Equal(t, "image/png", parts[1].contentType)
	assert.Contains(t, parts[1].disposition, "attachment")
	assert.Contains(t, parts[1].disposition, "q1-chart.png")

	got, err := decodeBase64Loose(string(parts[1].data))
	require.NoError(t, err)
	assert.Equal(t, pngBytes, got)
}

func TestSESOutputValidation(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	base := `
region: us-east-1
credentials:
  id: test-id
  secret: test-secret
from: "alerts@example.com"
to: ["bob@example.com"]
`

	tests := []struct {
		name    string
		extra   string
		wantErr string
	}{
		{
			name:    "no body",
			extra:   "",
			wantErr: "at least one of the fields 'text' or 'html'",
		},
		{
			name:    "image without source",
			extra:   "text: hi\nimages:\n  - content_id: logo\n",
			wantErr: "either 'path' or 'data' must be set",
		},
		{
			name:    "image without content id",
			extra:   "text: hi\nimages:\n  - data: aGk=\n",
			wantErr: "'content_id' is required",
		},
		{
			name:    "attachment without source",
			extra:   "text: hi\nattachments:\n  - filename: a.txt\n",
			wantErr: "either 'path' or 'data' must be set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pConf, err := sesOutputSpec().ParseYAML(base+tt.extra, nil)
			if err == nil {
				_, err = newSESWriter(pConf, service.MockResources())
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestSESOutputRuntimeErrors(t *testing.T) {
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	spec := sesOutputSpec()
	pConf, err := spec.ParseYAML(`
region: us-east-1
credentials:
  id: test-id
  secret: test-secret
from: "{{ .sender }}"
to: ["{{ .recipient }}"]
text: "hi"
`, nil)
	require.NoError(t, err)

	w, err := newSESWriter(pConf, service.MockResources())
	require.NoError(t, err)
	mock := &mockSESClient{}
	w.client = mock

	err = w.Write(context.Background(), service.NewMessage([]byte(`{"sender":"a@b.com"}`)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no recipients")
	assert.Equal(t, 0, mock.calls)
}

func TestBuildMIMEMessageStructure(t *testing.T) {
	raw, err := buildMIMEMessage(
		"alerts@example.com",
		[]string{"bob@example.com"},
		nil, nil,
		"Subject",
		"plain",
		"<p>html</p>",
		[]emailAttachment{{filename: "logo.png", contentType: "image/png", contentID: "logo", data: []byte("x")}},
		[]emailAttachment{{filename: "file.bin", contentType: "application/octet-stream", data: []byte("y")}},
	)
	require.NoError(t, err)

	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)

	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/mixed", mediaType)

	var parts []*partInfo
	collectParts(t, textproto.MIMEHeader(msg.Header), msg.Body, &parts)

	types := make([]string, 0, len(parts))
	for _, p := range parts {
		types = append(types, p.contentType)
	}
	assert.ElementsMatch(t,
		[]string{"text/plain", "text/html", "image/png", "application/octet-stream"},
		types,
	)

	// All leaf parts must be base64 encoded.
	for _, p := range parts {
		assert.Equal(t, "base64", p.encoding)
	}
	_ = params
}

func TestDecodeBase64Loose(t *testing.T) {
	b, err := decodeBase64Loose("aGVs\nbG8=")
	require.NoError(t, err)
	assert.Equal(t, "hello", string(b))

	b, err = decodeBase64Loose("aGVsbG8")
	require.NoError(t, err)
	assert.Equal(t, "hello", string(b))

	_, err = decodeBase64Loose("not valid %%")
	require.Error(t, err)
}

func TestTemplateHelpers(t *testing.T) {
	tpl, err := parseTemplate("test", `{{ .name | upper }}|{{ join "," .items }}|{{ .missing | default "fallback" }}|{{ base64 .raw }}`)
	require.NoError(t, err)

	out, err := render(tpl, map[string]any{
		"name":  "bob",
		"items": []any{"a", "b"},
		"raw":   "hi",
	})
	require.NoError(t, err)
	assert.Equal(t, "BOB|a,b|fallback|aGk=", out)
}

func TestSessionFieldsParsing(t *testing.T) {
	spec := service.NewConfigSpec().Fields(sessionFields()...)
	pConf, err := spec.ParseYAML(`
region: eu-west-1
endpoint: http://localhost:4566
credentials:
  profile: my-profile
  role: arn:aws:iam::123456789012:role/test
  role_external_id: ext-123
  expiry_window: 30s
`, nil)
	require.NoError(t, err)

	creds := pConf.Namespace("credentials")
	profile, err := creds.FieldString("profile")
	require.NoError(t, err)
	assert.Equal(t, "my-profile", profile)

	role, err := creds.FieldString("role")
	require.NoError(t, err)
	assert.Equal(t, "arn:aws:iam::123456789012:role/test", role)

	ext, err := creds.FieldString("role_external_id")
	require.NoError(t, err)
	assert.Equal(t, "ext-123", ext)

	window, err := creds.FieldDuration("expiry_window")
	require.NoError(t, err)
	assert.Equal(t, "30s", window.String())
}

func decodePart(t *testing.T, p *partInfo) string {
	t.Helper()
	require.Equal(t, "base64", p.encoding)
	decoded, err := decodeBase64Loose(string(p.data))
	require.NoError(t, err)
	return string(decoded)
}
