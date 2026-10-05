package ses

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"net/url"
	"sort"
	"strings"
	"time"
)

// emailAttachment is a fully resolved attachment or inline image.
type emailAttachment struct {
	filename    string
	contentType string
	// contentID is only set for inline images and is referenced from HTML via
	// `cid:<content_id>`.
	contentID string
	data      []byte
}

// mimePart is a single MIME entity. When the content type is multipart/* the
// body already contains the encoded sub-parts and boundaries.
type mimePart struct {
	header textproto.MIMEHeader
	body   []byte
}

func newTextPart(contentType, body string) mimePart {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", contentType)
	h.Set("Content-Transfer-Encoding", "base64")
	return mimePart{header: h, body: encodeBase64([]byte(body))}
}

func newAttachmentPart(a emailAttachment) mimePart {
	ct := a.contentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", ct)
	h.Set("Content-Transfer-Encoding", "base64")

	disposition := "attachment"
	if a.contentID != "" {
		disposition = "inline"
		h.Set("Content-ID", "<"+a.contentID+">")
	}
	h.Set("Content-Disposition", fmt.Sprintf("%s; %s", disposition, formatFilenameParam(a.filename)))

	return mimePart{header: h, body: encodeBase64(a.data)}
}

// buildMultipart assembles the given parts into a multipart entity and returns
// the resulting part, with the correct boundary reflected in its Content-Type.
func buildMultipart(subtype string, parts []mimePart) (mimePart, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		pw, err := w.CreatePart(p.header)
		if err != nil {
			return mimePart{}, err
		}
		if _, err := pw.Write(p.body); err != nil {
			return mimePart{}, err
		}
	}
	if err := w.Close(); err != nil {
		return mimePart{}, err
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", fmt.Sprintf("multipart/%s; boundary=%q", subtype, w.Boundary()))
	return mimePart{header: h, body: buf.Bytes()}, nil
}

// buildMIMEMessage renders a complete RFC 5322/MIME email, nesting
// multipart/alternative, multipart/related and multipart/mixed as required.
func buildMIMEMessage(
	from string,
	to, cc, replyTo []string,
	subject, textBody, htmlBody string,
	images, attachments []emailAttachment,
) ([]byte, error) {
	var textPart *mimePart
	if textBody != "" {
		p := newTextPart("text/plain; charset=utf-8", textBody)
		textPart = &p
	}

	imageParts := make([]mimePart, 0, len(images))
	for _, img := range images {
		imageParts = append(imageParts, newAttachmentPart(img))
	}

	attachmentParts := make([]mimePart, 0, len(attachments))
	for _, a := range attachments {
		attachmentParts = append(attachmentParts, newAttachmentPart(a))
	}

	// The HTML section, with inline images nested inside multipart/related.
	var htmlSection *mimePart
	if htmlBody != "" {
		htmlPart := newTextPart("text/html; charset=utf-8", htmlBody)
		if len(imageParts) > 0 {
			related, err := buildMultipart("related", append([]mimePart{htmlPart}, imageParts...))
			if err != nil {
				return nil, err
			}
			htmlSection = &related
		} else {
			htmlSection = &htmlPart
		}
	}

	var body mimePart
	switch {
	case textPart != nil && htmlSection != nil:
		alt, err := buildMultipart("alternative", []mimePart{*textPart, *htmlSection})
		if err != nil {
			return nil, err
		}
		body = alt
	case htmlSection != nil:
		body = *htmlSection
	case textPart != nil:
		if len(imageParts) > 0 {
			related, err := buildMultipart("related", append([]mimePart{*textPart}, imageParts...))
			if err != nil {
				return nil, err
			}
			body = related
		} else {
			body = *textPart
		}
	default:
		return nil, fmt.Errorf("at least one of the fields 'text' or 'html' must render to a non-empty body")
	}

	if len(attachmentParts) > 0 {
		mixed, err := buildMultipart("mixed", append([]mimePart{body}, attachmentParts...))
		if err != nil {
			return nil, err
		}
		body = mixed
	}

	var buf bytes.Buffer
	buf.WriteString("MIME-Version: 1.0\r\n")
	writeMailHeader(&buf, "From", formatAddress(from))
	writeMailHeader(&buf, "To", formatAddresses(to))
	if len(cc) > 0 {
		writeMailHeader(&buf, "Cc", formatAddresses(cc))
	}
	if len(replyTo) > 0 {
		writeMailHeader(&buf, "Reply-To", formatAddresses(replyTo))
	}
	writeMailHeader(&buf, "Subject", encodeHeaderValue(subject))
	writeMailHeader(&buf, "Date", time.Now().Format(time.RFC1123Z))

	keys := make([]string, 0, len(body.header))
	for k := range body.header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		writeMailHeader(&buf, k, body.header.Get(k))
	}

	buf.WriteString("\r\n")
	buf.Write(body.body)
	return buf.Bytes(), nil
}

func writeMailHeader(buf *bytes.Buffer, key, value string) {
	// Strip CR/LF to prevent header injection.
	value = strings.NewReplacer("\r", "", "\n", " ").Replace(value)
	buf.WriteString(key)
	buf.WriteString(": ")
	buf.WriteString(value)
	buf.WriteString("\r\n")
}

func encodeHeaderValue(v string) string {
	if isASCII(v) {
		return v
	}
	return mime.QEncoding.Encode("utf-8", v)
}

// formatAddresses renders a list of addresses as a single header value,
// RFC 2047 encoding any non-ASCII display names.
func formatAddresses(addrs []string) string {
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = formatAddress(a)
	}
	return strings.Join(out, ", ")
}

func formatAddress(addr string) string {
	if parsed, err := mail.ParseAddress(addr); err == nil {
		return parsed.String()
	}
	return addr
}

func formatFilenameParam(filename string) string {
	fallback := strings.Map(func(r rune) rune {
		if r > 127 {
			return '_'
		}
		return r
	}, filename)
	fallback = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(fallback)
	if isASCII(filename) {
		return fmt.Sprintf(`filename="%s"`, fallback)
	}
	return fmt.Sprintf(`filename="%s"; filename*=UTF-8''%s`, fallback, url.PathEscape(filename))
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// encodeBase64 base64-encodes data and wraps it at 76 characters with CRLF
// line endings, as required by MIME.
func encodeBase64(data []byte) []byte {
	encoded := base64.StdEncoding.EncodeToString(data)
	var buf bytes.Buffer
	for len(encoded) > 76 {
		buf.WriteString(encoded[:76])
		buf.WriteString("\r\n")
		encoded = encoded[76:]
	}
	buf.WriteString(encoded)
	buf.WriteString("\r\n")
	return buf.Bytes()
}
