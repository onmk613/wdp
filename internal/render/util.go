package render

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

func b64encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func b64decode(s string) string {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(b)
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
