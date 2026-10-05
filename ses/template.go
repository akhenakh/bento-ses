package ses

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"text/template"
	"time"

	"github.com/warpstreamlabs/bento/public/service"
)

// templateFuncs are extra helpers available to every Go template used by this
// component, on top of the built-in text/template functions.
var templateFuncs = template.FuncMap{
	"toJson": func(v any) (string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	},
	"base64": func(v any) string {
		switch s := v.(type) {
		case string:
			return base64.StdEncoding.EncodeToString([]byte(s))
		case []byte:
			return base64.StdEncoding.EncodeToString(s)
		default:
			return base64.StdEncoding.EncodeToString([]byte(fmt.Sprint(v)))
		}
	},
	"upper": strings.ToUpper,
	"lower": strings.ToLower,
	"trim":  strings.TrimSpace,
	"now":   time.Now,
	"formatTime": func(layout string, t time.Time) string {
		return t.Format(layout)
	},
	"replace": func(old, new, s string) string {
		return strings.ReplaceAll(s, old, new)
	},
	"join": func(sep string, v any) string {
		if v == nil {
			return ""
		}
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
			return fmt.Sprint(v)
		}
		parts := make([]string, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			parts[i] = fmt.Sprint(rv.Index(i).Interface())
		}
		return strings.Join(parts, sep)
	},
	// default mirrors the common sprig ordering: `{{ .x | default "y" }}`.
	"default": func(def any, given any) any {
		if isEmpty(given) {
			return def
		}
		return given
	},
}

func isEmpty(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String, reflect.Array, reflect.Slice, reflect.Map:
		return rv.Len() == 0
	case reflect.Bool:
		return !rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return rv.Float() == 0
	case reflect.Ptr, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

func parseTemplate(name, s string) (*template.Template, error) {
	t, err := template.New(name).Funcs(templateFuncs).Option("missingkey=zero").Parse(s)
	if err != nil {
		return nil, fmt.Errorf("parsing template %q: %w", name, err)
	}
	return t, nil
}

func parseTemplateList(prefix string, strs []string) ([]*template.Template, error) {
	out := make([]*template.Template, 0, len(strs))
	for i, s := range strs {
		t, err := parseTemplate(fmt.Sprintf("%s[%d]", prefix, i), s)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func render(t *template.Template, data any) (string, error) {
	if t == nil {
		return "", nil
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	// With missingkey=zero, absent map keys render as "<no value>". Treat
	// those as empty strings so optional fields behave intuitively, while the
	// `default` helper still works because it receives the underlying nil.
	return strings.ReplaceAll(buf.String(), "<no value>", ""), nil
}

func renderList(ts []*template.Template, data any) ([]string, error) {
	if len(ts) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		s, err := render(t, data)
		if err != nil {
			return nil, err
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// messageData extracts the template data context from a message. JSON objects
// are exposed as-is (so `{{ .field }}` works), any other JSON value is wrapped
// under `value`, and non-JSON payloads are exposed as `value`/`raw` strings.
func messageData(msg *service.Message) (any, error) {
	b, err := msg.AsBytes()
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(b, &v); err == nil {
		if _, ok := v.(map[string]any); ok {
			return v, nil
		}
		return map[string]any{"value": v}, nil
	}
	return map[string]any{"value": string(b), "raw": string(b)}, nil
}
