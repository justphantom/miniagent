package tools

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// decodeWebBody converts the raw response body to a UTF-8 string using charset
// detection from the Content-Type header or HTML <meta charset> tag. Supported
// conversions: UTF-8 (passthrough, BOM stripped), UTF-16 (BOM or declared
// endianness), Latin-1 / Windows-125x family (identity byte→rune). For unsupported
// multi-byte charsets (GBK, Big5, Shift_JIS, …) the body is passed through raw
// with a warning so the LLM sees the degradation reason rather than silent mojibake.
func decodeWebBody(ctype, body string) (out, warning string) {
	// BOM takes precedence over any declared charset (matches browser behavior).
	if dec, ok := decodeBOM(body); ok {
		return dec, ""
	}

	cs := charsetFromContentType(ctype)
	if cs == "" {
		cs = charsetFromMeta(body)
	}

	if cs == "" {
		// No declaration: UTF-8 is the web default. Validate before assuming.
		if utf8.ValidString(body) {
			return strings.TrimPrefix(body, "\uFEFF"), ""
		}
		return latin1Decode(body), "charset unknown; decoded as latin-1 (may be garbled)"
	}

	cs = strings.ToLower(strings.TrimSpace(cs))
	switch {
	case cs == "utf-8" || cs == "utf8" || cs == "us-ascii" || cs == "ascii":
		if utf8.ValidString(body) {
			return strings.TrimPrefix(body, "\uFEFF"), ""
		}
		return latin1Decode(body), "declared " + cs + " but body is not valid UTF-8; decoded as latin-1"
	case cs == "utf-16le" || cs == "utf-16":
		return decodeUTF16(body, false), ""
	case cs == "utf-16be":
		return decodeUTF16(body, true), ""
	case isLatin1(cs):
		return latin1Decode(body), ""
	default:
		// Declared but unsupported (GBK, Big5, Shift_JIS, …). Try UTF-8 in case
		// the declaration is wrong; otherwise warn + raw passthrough.
		if utf8.ValidString(body) {
			return body, "declared charset=" + cs + " but body is valid UTF-8; used as-is"
		}
		return body, "charset=" + cs + " not supported; raw passthrough (may be garbled)"
	}
}

// charsetFromContentType extracts the charset= value from a Content-Type header.
func charsetFromContentType(ctype string) string {
	for part := range strings.SplitSeq(ctype, ";") {
		part = strings.TrimSpace(part)
		name, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(name, "charset") {
			return strings.Trim(val, "\"'")
		}
	}
	return ""
}

// charsetFromMeta sniffs the first 512 bytes for an HTML <meta charset> or
// <meta http-equiv content-type> declaration.
func charsetFromMeta(body string) string {
	head := strings.ToLower(body[:min(len(body), 512)])
	idx := strings.Index(head, "<meta")
	if idx < 0 {
		return ""
	}
	end := strings.IndexByte(head[idx:], '>')
	if end < 0 {
		return ""
	}
	tag := head[idx : idx+end]
	if _, after, ok := strings.Cut(tag, "charset"); ok {
		rest := strings.TrimLeft(after, "= \"'")
		for k, c := range rest {
			if c == '"' || c == '\'' || c == ' ' || c == '>' {
				return rest[:k]
			}
		}
		return rest
	}
	return ""
}

// decodeBOM decodes a body that starts with a UTF-16 BOM. Returns false for
// UTF-8 BOM (handled by prefix trimming in the caller) or no BOM.
func decodeBOM(body string) (string, bool) {
	if strings.HasPrefix(body, "\xFF\xFE") || strings.HasPrefix(body, "\xFE\xFF") {
		return decodeUTF16(body, false), true
	}
	return "", false
}

// decodeUTF16 decodes a UTF-16 byte string, using BOM for endianness or bigEndian
// when no BOM is present.
func decodeUTF16(b string, bigEndian bool) string {
	if strings.HasPrefix(b, "\xFF\xFE") {
		b, bigEndian = b[2:], false
	} else if strings.HasPrefix(b, "\xFE\xFF") {
		b, bigEndian = b[2:], true
	}
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	u16 := make([]uint16, len(b)/2)
	for i := 0; i < len(b); i += 2 {
		if bigEndian {
			u16[i/2] = uint16(b[i])<<8 | uint16(b[i+1])
		} else {
			u16[i/2] = uint16(b[i+1])<<8 | uint16(b[i])
		}
	}
	return string(utf16.Decode(u16))
}

// latin1Decode maps each byte to its rune value (ISO-8859-1 identity). Correct for
// the 0x00-0x7F range; readable for Western European 0x80-0xFF (Windows-1252 smart
// quotes in 0x80-0x9F render as C1 controls — imperfect but lossless). Byte-wise
// iteration is required: range-over-string would yield U+FFFD for non-UTF-8 bytes.
func latin1Decode(b string) string {
	runes := make([]rune, len(b))
	for i := range len(b) {
		runes[i] = rune(b[i])
	}
	return string(runes)
}

// isLatin1 reports whether cs decodes correctly via the identity byte→rune
// mapping: only iso-8859-1 (and its windows-1252 near-equivalent) qualify.
// Other legacy single-byte sets (koi8-r, windows-1251, …) would mojibake silently
// under identity mapping, so they fall through to the warn+passthrough path.
func isLatin1(cs string) bool {
	switch cs {
	case "iso-8859-1", "latin1", "windows-1252", "iso-8859-15":
		return true
	}
	return false
}
