package provider

import (
	"encoding/json"
	"io"
	"net/http"
)

// ExplicitCacheControlHTTPClient marks the end of the stable system content as
// an ephemeral cache breakpoint. Adapters must enable this only where their
// documented OpenAI-compatible protocol accepts cache_control blocks.
func ExplicitCacheControlHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = &http.Client{}
	}
	clone := *base
	transport := clone.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if _, ok := transport.(*explicitCacheControlTransport); !ok {
		clone.Transport = &explicitCacheControlTransport{base: transport}
	}
	return &clone
}

type explicitCacheControlTransport struct {
	base http.RoundTripper
}

func (t *explicitCacheControlTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body == nil || request.Method != http.MethodPost {
		return t.base.RoundTrip(request)
	}
	reader, writer := io.Pipe()
	go func() {
		defer request.Body.Close()
		err := streamSystemCacheBreakpoint(request.Body, writer)
		_ = writer.CloseWithError(err)
	}()
	clone := request.Clone(request.Context())
	clone.Body = reader
	clone.GetBody = nil
	clone.ContentLength = -1
	clone.Header = request.Header.Clone()
	clone.Header.Del("Content-Length")
	return t.base.RoundTrip(clone)
}

// streamSystemCacheBreakpoint rewrites the first system message token by
// token. Large user messages and base64 attachments flow through the pipe
// without a second request-sized allocation.
func streamSystemCacheBreakpoint(source io.Reader, target io.Writer) error {
	decoder := json.NewDecoder(source)
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := first.(json.Delim)
	if !ok || delimiter != '{' {
		return writeJSONStreamValue(decoder, target, first)
	}
	if _, err := io.WriteString(target, "{"); err != nil {
		return err
	}
	field := 0
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, _ := keyToken.(string)
		if field > 0 {
			if _, err := io.WriteString(target, ","); err != nil {
				return err
			}
		}
		if err := writeJSONScalar(target, key); err != nil {
			return err
		}
		if _, err := io.WriteString(target, ":"); err != nil {
			return err
		}
		value, err := decoder.Token()
		if err != nil {
			return err
		}
		if key == "messages" {
			var copiedRest bool
			copiedRest, err = writeMessagesWithCacheBreakpoint(decoder, source, target, value)
			if copiedRest {
				return err
			}
		} else {
			err = writeJSONStreamValue(decoder, target, value)
		}
		if err != nil {
			return err
		}
		field++
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	_, err = io.WriteString(target, "}")
	return err
}

func writeMessagesWithCacheBreakpoint(decoder *json.Decoder, source io.Reader, target io.Writer, first any) (bool, error) {
	delimiter, ok := first.(json.Delim)
	if !ok || delimiter != '[' {
		return false, writeJSONStreamValue(decoder, target, first)
	}
	if _, err := io.WriteString(target, "["); err != nil {
		return false, err
	}
	if decoder.More() {
		value, err := decoder.Token()
		if err != nil {
			return false, err
		}
		err = writeFirstMessageWithCacheBreakpoint(decoder, target, value)
		if err != nil {
			return false, err
		}
	}
	// The breakpoint can only affect the first stable system message. Copy the
	// unread decoder buffer and underlying source byte-for-byte from here so
	// later image/base64 strings are never materialized by the transformer.
	_, err := io.Copy(target, io.MultiReader(decoder.Buffered(), source))
	return true, err
}

func writeFirstMessageWithCacheBreakpoint(decoder *json.Decoder, target io.Writer, first any) error {
	delimiter, ok := first.(json.Delim)
	if !ok || delimiter != '{' {
		return writeJSONStreamValue(decoder, target, first)
	}
	if _, err := io.WriteString(target, "{"); err != nil {
		return err
	}
	role := ""
	field := 0
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, _ := keyToken.(string)
		if field > 0 {
			if _, err := io.WriteString(target, ","); err != nil {
				return err
			}
		}
		if err := writeJSONScalar(target, key); err != nil {
			return err
		}
		if _, err := io.WriteString(target, ":"); err != nil {
			return err
		}
		value, err := decoder.Token()
		if err != nil {
			return err
		}
		if key == "role" {
			role, _ = value.(string)
		}
		if key == "content" && role == "system" {
			if content, ok := value.(string); ok && content != "" {
				if _, err := io.WriteString(target, `[{"type":"text","text":`); err != nil {
					return err
				}
				if err := writeJSONScalar(target, content); err != nil {
					return err
				}
				if _, err := io.WriteString(target, `,"cache_control":{"type":"ephemeral"}}]`); err != nil {
					return err
				}
				field++
				continue
			}
		}
		if err := writeJSONStreamValue(decoder, target, value); err != nil {
			return err
		}
		field++
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	_, err := io.WriteString(target, "}")
	return err
}

func writeJSONStreamValue(decoder *json.Decoder, target io.Writer, first any) error {
	delimiter, isDelimiter := first.(json.Delim)
	if !isDelimiter {
		return writeJSONScalar(target, first)
	}
	switch delimiter {
	case '{':
		if _, err := io.WriteString(target, "{"); err != nil {
			return err
		}
		field := 0
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			if field > 0 {
				if _, err := io.WriteString(target, ","); err != nil {
					return err
				}
			}
			if err := writeJSONScalar(target, key); err != nil {
				return err
			}
			if _, err := io.WriteString(target, ":"); err != nil {
				return err
			}
			value, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := writeJSONStreamValue(decoder, target, value); err != nil {
				return err
			}
			field++
		}
		if _, err := decoder.Token(); err != nil {
			return err
		}
		_, err := io.WriteString(target, "}")
		return err
	case '[':
		if _, err := io.WriteString(target, "["); err != nil {
			return err
		}
		index := 0
		for decoder.More() {
			if index > 0 {
				if _, err := io.WriteString(target, ","); err != nil {
					return err
				}
			}
			value, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := writeJSONStreamValue(decoder, target, value); err != nil {
				return err
			}
			index++
		}
		if _, err := decoder.Token(); err != nil {
			return err
		}
		_, err := io.WriteString(target, "]")
		return err
	default:
		return writeJSONScalar(target, first)
	}
}

func writeJSONScalar(target io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = target.Write(encoded)
	return err
}

func addSystemCacheBreakpoint(raw []byte) ([]byte, bool) {
	var request map[string]any
	if json.Unmarshal(raw, &request) != nil {
		return raw, false
	}
	messages, ok := request["messages"].([]any)
	if !ok {
		return raw, false
	}
	for _, candidate := range messages {
		message, ok := candidate.(map[string]any)
		if !ok || message["role"] != "system" {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			if content == "" {
				return raw, false
			}
			message["content"] = []any{map[string]any{
				"type": "text", "text": content,
				"cache_control": map[string]any{"type": "ephemeral"},
			}}
		case []any:
			for index := len(content) - 1; index >= 0; index-- {
				block, ok := content[index].(map[string]any)
				if !ok || block["type"] != "text" {
					continue
				}
				if _, exists := block["cache_control"]; !exists {
					block["cache_control"] = map[string]any{"type": "ephemeral"}
				}
				encoded, err := json.Marshal(request)
				return encoded, err == nil
			}
			return raw, false
		default:
			return raw, false
		}
		encoded, err := json.Marshal(request)
		return encoded, err == nil
	}
	return raw, false
}
