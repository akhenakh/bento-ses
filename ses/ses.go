package ses

import (
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/warpstreamlabs/bento/public/service"
)

const (
	fieldFrom             = "from"
	fieldTo               = "to"
	fieldCC               = "cc"
	fieldBCC              = "bcc"
	fieldReplyTo          = "reply_to"
	fieldSubject          = "subject"
	fieldText             = "text"
	fieldHTML             = "html"
	fieldImages           = "images"
	fieldAttachments      = "attachments"
	fieldConfigurationSet = "configuration_set"
	fieldTimeout          = "timeout"

	fieldContentID   = "content_id"
	fieldFilename    = "filename"
	fieldContentType = "content_type"
	fieldPath        = "path"
	fieldData        = "data"
)

func sesOutputSpec() *service.ConfigSpec {
	return service.NewConfigSpec().
		Stable().
		Categories("Services", "AWS").
		Summary("Sends an email through Amazon SES using the SES v2 API.").
		Description(`
Sends an email for each message via the [Amazon SES v2 SendEmail API](https://docs.aws.amazon.com/ses/latest/APIReference-V2/API_SendEmail.html).

All string fields (`+"`from`"+`, `+"`to`"+`, `+"`cc`"+`, `+"`bcc`"+`, `+"`reply_to`"+`, `+"`subject`"+`, `+"`text`"+` and `+"`html`"+`) are processed as [Go templates](https://pkg.go.dev/text/template) where the message payload is the data context. When the payload is a JSON object its fields are available directly, e.g. `+"`{{ .customer.email }}`"+`. A small set of helper functions is available in addition to the built-ins: `+"`toJson`"+`, `+"`base64`"+`, `+"`upper`"+`, `+"`lower`"+`, `+"`trim`"+`, `+"`join`"+`, `+"`replace`"+`, `+"`default`"+`, `+"`now`"+` and `+"`formatTime`"+`.

Inline images and attachments can either be read from a file with `+"`path`"+` (itself a template) or provided inline as base64 with `+"`data`"+` (also a template, e.g. `+"`{{ base64 .report }}`"+`). Inline images are embedded with a `+"`Content-ID`"+` and can be referenced from the HTML body with `+"`<img src=\"cid:my_id\">`"+`.

The message is assembled as a raw MIME email so that HTML, inline images and attachments are all supported.`).
		Footnotes(`
## Credentials

By default Bento will use a shared credentials file when connecting to AWS services. It's also possible to set them explicitly at the component level, allowing you to transfer data across accounts. You can find out more [in this document](/docs/guides/cloud/aws).
`+service.OutputPerformanceDocs(true, false)).
		Fields(
			service.NewStringField(fieldFrom).
				Description("The sender email address, rendered as a Go template. This must be a verified identity in Amazon SES.").
				Example("alerts@example.com"),
			service.NewStringListField(fieldTo).
				Description("A list of recipient email addresses, each rendered as a Go template.").
				Example([]string{"{{ .email }}"}),
			service.NewStringListField(fieldCC).
				Description("A list of carbon copy recipient email addresses, each rendered as a Go template.").
				Optional().
				Advanced(),
			service.NewStringListField(fieldBCC).
				Description("A list of blind carbon copy recipient email addresses, each rendered as a Go template.").
				Optional().
				Advanced(),
			service.NewStringListField(fieldReplyTo).
				Description("A list of reply-to email addresses, each rendered as a Go template.").
				Optional().
				Advanced(),
			service.NewStringField(fieldSubject).
				Description("The email subject, rendered as a Go template.").
				Optional().
				Example("Alert for {{ .site }}"),
			service.NewStringField(fieldText).
				Description("The plain text body of the email, rendered as a Go template. At least one of `text` or `html` must be provided.").
				Optional(),
			service.NewStringField(fieldHTML).
				Description("The HTML body of the email, rendered as a Go template. At least one of `text` or `html` must be provided.").
				Optional().
				Example(`<h1>Hello {{ .name }}</h1><img src="cid:logo">`),
			service.NewObjectListField(fieldImages,
				service.NewStringField(fieldContentID).
					Description("The Content-ID used to reference this image from the HTML body via `cid:<content_id>`."),
				service.NewStringField(fieldFilename).
					Description("The filename of the image, rendered as a Go template. Used to infer the content type when it is not set.").
					Optional(),
				service.NewStringField(fieldContentType).
					Description("The content type of the image. When omitted it is inferred from the filename.").
					Optional().
					Example("image/png"),
				service.NewStringField(fieldPath).
					Description("A path to a file to read the image bytes from, rendered as a Go template. Either `path` or `data` must be set. Take care when this template derives from untrusted input, as it allows reading arbitrary files from disk.").
					Optional(),
				service.NewStringField(fieldData).
					Description("Base64 encoded image bytes, rendered as a Go template. Either `path` or `data` must be set.").
					Optional().
					Secret(),
			).
				Description("A list of images to embed inline in the HTML body using a Content-ID.").
				Optional().
				Advanced(),
			service.NewObjectListField(fieldAttachments,
				service.NewStringField(fieldFilename).
					Description("The filename of the attachment, rendered as a Go template. Used to infer the content type when it is not set.").
					Optional(),
				service.NewStringField(fieldContentType).
					Description("The content type of the attachment. When omitted it is inferred from the filename.").
					Optional().
					Example("application/pdf"),
				service.NewStringField(fieldPath).
					Description("A path to a file to read the attachment bytes from, rendered as a Go template. Either `path` or `data` must be set. Take care when this template derives from untrusted input, as it allows reading arbitrary files from disk.").
					Optional(),
				service.NewStringField(fieldData).
					Description("Base64 encoded attachment bytes, rendered as a Go template. Either `path` or `data` must be set.").
					Optional().
					Secret(),
			).
				Description("A list of files to attach to the email.").
				Optional().
				Advanced(),
			service.NewStringField(fieldConfigurationSet).
				Description("An optional Amazon SES configuration set to use when sending the email.").
				Optional().
				Advanced(),
			service.NewDurationField(fieldTimeout).
				Description("The maximum period to wait on a send before abandoning it and reattempting.").
				Advanced().
				Default("10s"),
			service.NewOutputMaxInFlightField(),
		).
		Fields(sessionFields()...)
}

func init() {
	err := service.RegisterOutput("aws_ses", sesOutputSpec(),
		func(conf *service.ParsedConfig, mgr *service.Resources) (service.Output, int, error) {
			maxInFlight, err := conf.FieldMaxInFlight()
			if err != nil {
				return nil, 0, err
			}
			w, err := newSESWriter(conf, mgr)
			if err != nil {
				return nil, 0, err
			}
			return w, maxInFlight, nil
		})
	if err != nil {
		panic(err)
	}
}

// sesAPI is the subset of the SES v2 client used by the component. It exists so
// that the writer can be tested without hitting AWS.
type sesAPI interface {
	SendEmail(ctx context.Context, params *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

type imageSpec struct {
	contentID   string
	filename    *template.Template
	contentType string
	path        *template.Template
	data        *template.Template
}

type attachmentSpec struct {
	filename    *template.Template
	contentType string
	path        *template.Template
	data        *template.Template
}

type sesConfig struct {
	aconf            aws.Config
	timeout          time.Duration
	configurationSet string

	from    *template.Template
	to      []*template.Template
	cc      []*template.Template
	bcc     []*template.Template
	replyTo []*template.Template

	subject *template.Template
	text    *template.Template
	html    *template.Template

	images      []imageSpec
	attachments []attachmentSpec
}

func sesConfigFromParsed(conf *service.ParsedConfig) (sesConfig, error) {
	var c sesConfig
	var err error

	if c.aconf, err = getSession(context.Background(), conf); err != nil {
		return c, err
	}
	if c.timeout, err = conf.FieldDuration(fieldTimeout); err != nil {
		return c, err
	}
	if conf.Contains(fieldConfigurationSet) {
		c.configurationSet, _ = conf.FieldString(fieldConfigurationSet)
	}

	fromStr, err := conf.FieldString(fieldFrom)
	if err != nil {
		return c, err
	}
	if c.from, err = parseTemplate(fieldFrom, fromStr); err != nil {
		return c, err
	}

	toStrs, err := conf.FieldStringList(fieldTo)
	if err != nil {
		return c, err
	}
	if c.to, err = parseTemplateList(fieldTo, toStrs); err != nil {
		return c, err
	}

	if c.cc, err = optionalTemplateList(conf, fieldCC); err != nil {
		return c, err
	}
	if c.bcc, err = optionalTemplateList(conf, fieldBCC); err != nil {
		return c, err
	}
	if c.replyTo, err = optionalTemplateList(conf, fieldReplyTo); err != nil {
		return c, err
	}

	if c.subject, err = optionalTemplate(conf, fieldSubject); err != nil {
		return c, err
	}
	if c.text, err = optionalTemplate(conf, fieldText); err != nil {
		return c, err
	}
	if c.html, err = optionalTemplate(conf, fieldHTML); err != nil {
		return c, err
	}

	if c.images, err = parseImages(conf); err != nil {
		return c, err
	}
	if c.attachments, err = parseAttachments(conf); err != nil {
		return c, err
	}

	if c.text == nil && c.html == nil {
		return c, fmt.Errorf("at least one of the fields '%s' or '%s' must be provided", fieldText, fieldHTML)
	}

	return c, nil
}

func optionalTemplate(conf *service.ParsedConfig, field string) (*template.Template, error) {
	if !conf.Contains(field) {
		return nil, nil
	}
	s, err := conf.FieldString(field)
	if err != nil {
		return nil, err
	}
	if s == "" {
		return nil, nil
	}
	return parseTemplate(field, s)
}

func optionalTemplateList(conf *service.ParsedConfig, field string) ([]*template.Template, error) {
	if !conf.Contains(field) {
		return nil, nil
	}
	strs, err := conf.FieldStringList(field)
	if err != nil {
		return nil, err
	}
	return parseTemplateList(field, strs)
}

func parseImages(conf *service.ParsedConfig) ([]imageSpec, error) {
	if !conf.Contains(fieldImages) {
		return nil, nil
	}
	objs, err := conf.FieldObjectList(fieldImages)
	if err != nil {
		return nil, err
	}
	out := make([]imageSpec, 0, len(objs))
	for i, obj := range objs {
		var spec imageSpec
		prefix := fmt.Sprintf("%s[%d]", fieldImages, i)

		if spec.contentID, err = obj.FieldString(fieldContentID); err != nil {
			return nil, fmt.Errorf("%s: %w", prefix, err)
		}
		if spec.contentID == "" {
			return nil, fmt.Errorf("%s: '%s' is required", prefix, fieldContentID)
		}
		if spec.filename, err = optionalTemplate(obj, fieldFilename); err != nil {
			return nil, fmt.Errorf("%s: %w", prefix, err)
		}
		if obj.Contains(fieldContentType) {
			spec.contentType, _ = obj.FieldString(fieldContentType)
		}
		if spec.path, err = optionalTemplate(obj, fieldPath); err != nil {
			return nil, fmt.Errorf("%s: %w", prefix, err)
		}
		if spec.data, err = optionalTemplate(obj, fieldData); err != nil {
			return nil, fmt.Errorf("%s: %w", prefix, err)
		}
		if spec.path == nil && spec.data == nil {
			return nil, fmt.Errorf("%s: either '%s' or '%s' must be set", prefix, fieldPath, fieldData)
		}
		out = append(out, spec)
	}
	return out, nil
}

func parseAttachments(conf *service.ParsedConfig) ([]attachmentSpec, error) {
	if !conf.Contains(fieldAttachments) {
		return nil, nil
	}
	objs, err := conf.FieldObjectList(fieldAttachments)
	if err != nil {
		return nil, err
	}
	out := make([]attachmentSpec, 0, len(objs))
	for i, obj := range objs {
		var spec attachmentSpec
		prefix := fmt.Sprintf("%s[%d]", fieldAttachments, i)

		if spec.filename, err = optionalTemplate(obj, fieldFilename); err != nil {
			return nil, fmt.Errorf("%s: %w", prefix, err)
		}
		if obj.Contains(fieldContentType) {
			spec.contentType, _ = obj.FieldString(fieldContentType)
		}
		if spec.path, err = optionalTemplate(obj, fieldPath); err != nil {
			return nil, fmt.Errorf("%s: %w", prefix, err)
		}
		if spec.data, err = optionalTemplate(obj, fieldData); err != nil {
			return nil, fmt.Errorf("%s: %w", prefix, err)
		}
		if spec.path == nil && spec.data == nil {
			return nil, fmt.Errorf("%s: either '%s' or '%s' must be set", prefix, fieldPath, fieldData)
		}
		out = append(out, spec)
	}
	return out, nil
}

type sesWriter struct {
	conf   sesConfig
	client sesAPI
	log    *service.Logger

	readFile func(string) ([]byte, error)
}

func newSESWriter(conf *service.ParsedConfig, mgr *service.Resources) (*sesWriter, error) {
	var c sesConfig
	var err error
	if c, err = sesConfigFromParsed(conf); err != nil {
		return nil, err
	}
	return &sesWriter{
		conf:     c,
		log:      mgr.Logger(),
		readFile: os.ReadFile,
	}, nil
}

func (w *sesWriter) Connect(ctx context.Context) error {
	if w.client != nil {
		return nil
	}
	w.client = sesv2.NewFromConfig(w.conf.aconf)
	return nil
}

func (w *sesWriter) Write(ctx context.Context, msg *service.Message) error {
	if w.client == nil {
		return service.ErrNotConnected
	}

	ctx, cancel := context.WithTimeout(ctx, w.conf.timeout)
	defer cancel()

	data, err := messageData(msg)
	if err != nil {
		return fmt.Errorf("extracting message payload: %w", err)
	}

	from, err := render(w.conf.from, data)
	if err != nil {
		return fmt.Errorf("rendering '%s': %w", fieldFrom, err)
	}
	if from = strings.TrimSpace(from); from == "" {
		return fmt.Errorf("'%s' rendered to an empty address", fieldFrom)
	}

	to, err := renderList(w.conf.to, data)
	if err != nil {
		return fmt.Errorf("rendering '%s': %w", fieldTo, err)
	}
	if len(to) == 0 {
		return fmt.Errorf("'%s' rendered to no recipients", fieldTo)
	}

	cc, err := renderList(w.conf.cc, data)
	if err != nil {
		return fmt.Errorf("rendering '%s': %w", fieldCC, err)
	}
	bcc, err := renderList(w.conf.bcc, data)
	if err != nil {
		return fmt.Errorf("rendering '%s': %w", fieldBCC, err)
	}
	replyTo, err := renderList(w.conf.replyTo, data)
	if err != nil {
		return fmt.Errorf("rendering '%s': %w", fieldReplyTo, err)
	}

	subject, err := render(w.conf.subject, data)
	if err != nil {
		return fmt.Errorf("rendering '%s': %w", fieldSubject, err)
	}
	textBody, err := render(w.conf.text, data)
	if err != nil {
		return fmt.Errorf("rendering '%s': %w", fieldText, err)
	}
	htmlBody, err := render(w.conf.html, data)
	if err != nil {
		return fmt.Errorf("rendering '%s': %w", fieldHTML, err)
	}

	images, err := w.resolveImages(data)
	if err != nil {
		return err
	}
	attachments, err := w.resolveAttachments(data)
	if err != nil {
		return err
	}

	raw, err := buildMIMEMessage(from, to, cc, replyTo, subject, textBody, htmlBody, images, attachments)
	if err != nil {
		return fmt.Errorf("building MIME message: %w", err)
	}

	input := &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(from),
		Destination: &types.Destination{
			ToAddresses:  to,
			CcAddresses:  cc,
			BccAddresses: bcc,
		},
		ReplyToAddresses: replyTo,
		Content: &types.EmailContent{
			Raw: &types.RawMessage{Data: raw},
		},
	}
	if w.conf.configurationSet != "" {
		input.ConfigurationSetName = aws.String(w.conf.configurationSet)
	}

	if _, err := w.client.SendEmail(ctx, input); err != nil {
		return fmt.Errorf("sending email via SES: %w", err)
	}
	w.log.Debugf("Sent email via SES to %v", to)
	return nil
}

func (w *sesWriter) Close(ctx context.Context) error {
	w.client = nil
	return nil
}

func (w *sesWriter) resolveImages(data any) ([]emailAttachment, error) {
	out := make([]emailAttachment, 0, len(w.conf.images))
	for i, spec := range w.conf.images {
		a, err := w.resolveOne(spec.filename, spec.contentType, spec.path, spec.data, data)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", fieldImages, i, err)
		}
		a.contentID = spec.contentID
		out = append(out, a)
	}
	return out, nil
}

func (w *sesWriter) resolveAttachments(data any) ([]emailAttachment, error) {
	out := make([]emailAttachment, 0, len(w.conf.attachments))
	for i, spec := range w.conf.attachments {
		a, err := w.resolveOne(spec.filename, spec.contentType, spec.path, spec.data, data)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", fieldAttachments, i, err)
		}
		out = append(out, a)
	}
	return out, nil
}

func (w *sesWriter) resolveOne(
	filenameT *template.Template,
	contentType string,
	pathT, dataT *template.Template,
	data any,
) (emailAttachment, error) {
	filename, err := render(filenameT, data)
	if err != nil {
		return emailAttachment{}, fmt.Errorf("rendering '%s': %w", fieldFilename, err)
	}
	filename = strings.TrimSpace(filename)
	if filename == "" {
		return emailAttachment{}, fmt.Errorf("'%s' rendered to an empty value", fieldFilename)
	}

	var content []byte
	switch {
	case pathT != nil:
		path, err := render(pathT, data)
		if err != nil {
			return emailAttachment{}, fmt.Errorf("rendering '%s': %w", fieldPath, err)
		}
		path = strings.TrimSpace(path)
		if content, err = w.readFile(path); err != nil {
			return emailAttachment{}, fmt.Errorf("reading %q: %w", path, err)
		}
	case dataT != nil:
		encoded, err := render(dataT, data)
		if err != nil {
			return emailAttachment{}, fmt.Errorf("rendering '%s': %w", fieldData, err)
		}
		if content, err = decodeBase64Loose(encoded); err != nil {
			return emailAttachment{}, fmt.Errorf("decoding '%s': %w", fieldData, err)
		}
	default:
		return emailAttachment{}, fmt.Errorf("either '%s' or '%s' must be set", fieldPath, fieldData)
	}

	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(filename))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	return emailAttachment{
		filename:    filename,
		contentType: contentType,
		data:        content,
	}, nil
}

func decodeBase64Loose(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, s)
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("invalid base64 data")
}
