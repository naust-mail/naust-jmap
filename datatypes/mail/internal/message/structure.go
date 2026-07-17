package message

import (
	"bytes"
	"mime"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// maxMultipartDepth bounds multipart recursion so hostile input cannot
// blow the stack; parts nested deeper are treated as having no children.
const maxMultipartDepth = 64

// maxParts bounds the total number of body parts a single message yields.
// maxMultipartDepth caps nesting depth, but not breadth: a bounded-size
// message can still declare millions of tiny sibling parts, and one Part
// struct per part balloons a small upload into hundreds of megabytes of
// heap (roughly 15x). A real message has at most dozens of parts, so this
// cap is far above anything legitimate; parts beyond it are dropped, the
// same best-effort truncation maxMultipartDepth uses for over-deep nesting.
const maxParts = 10000

// contentType returns the canonical media type (lowercase, parameters
// stripped), its parameters, and whether a Content-Type header field was
// present at all. Unparseable values fall back to a bare-token reading,
// then to defaultType.
func contentType(headers []HeaderField, defaultType string) (string, map[string]string, bool) {
	raw, ok := headerLast(headers, "Content-Type")
	if !ok {
		return defaultType, nil, false
	}
	mt, params, err := mime.ParseMediaType(strings.TrimSpace(unfold(raw)))
	if err == nil && strings.Contains(mt, "/") {
		return mt, params, true
	}
	clean, _ := stripComments(unfold(raw))
	if semi := strings.IndexByte(clean, ';'); semi >= 0 {
		clean = clean[:semi]
	}
	clean = strings.ToLower(removeWSP(clean))
	if strings.Contains(clean, "/") {
		return clean, nil, true
	}
	return defaultType, nil, false
}

// charsetOf implements the charset rules of RFC 8621 4.1.4: the charset
// parameter if present; the implicit "us-ascii" when there is no
// Content-Type header field or it is text/* without a charset parameter;
// null when the header field is present but not text/*.
func charsetOf(typ string, params map[string]string, ctPresent bool) *string {
	if cs, ok := params["charset"]; ok && cs != "" {
		return &cs
	}
	if !ctPresent || strings.HasPrefix(typ, "text/") {
		ascii := "us-ascii"
		return &ascii
	}
	return nil
}

// dispositionOf returns the Content-Disposition value (lowercase,
// parameters stripped) and the decoded file name: the disposition's
// "filename" parameter (RFC 2231, handled by mime.ParseMediaType) or,
// failing that, the Content-Type "name" parameter.
func dispositionOf(headers []HeaderField, ctParams map[string]string) (*string, *string) {
	var disp *string
	var filename string
	if raw, ok := headerLast(headers, "Content-Disposition"); ok {
		if v, params, err := mime.ParseMediaType(strings.TrimSpace(unfold(raw))); err == nil {
			disp = &v
			filename = params["filename"]
		} else {
			clean, _ := stripComments(unfold(raw))
			if semi := strings.IndexByte(clean, ';'); semi >= 0 {
				clean = clean[:semi]
			}
			if clean = strings.ToLower(strings.TrimSpace(clean)); clean != "" {
				disp = &clean
			}
		}
	}
	if filename == "" {
		filename = ctParams["name"]
	}
	if filename == "" {
		return disp, nil
	}
	// RFC 2047 encoding inside the parameter value is illegal but common.
	name := norm.NFC.String(decodeWords(filename))
	return disp, &name
}

func cteOf(headers []HeaderField) string {
	raw, ok := headerLast(headers, "Content-Transfer-Encoding")
	if !ok {
		return ""
	}
	clean, _ := stripComments(unfold(raw))
	return strings.ToLower(strings.TrimSpace(clean))
}

// angleValue reads a header field whose value is CFWS plus an
// angle-bracketed token (Content-Id), returning the token without
// brackets, or nil.
func angleValue(headers []HeaderField, name string) *string {
	raw, ok := headerLast(headers, name)
	if !ok {
		return nil
	}
	clean, _ := stripComments(unfold(raw))
	v := strings.TrimSpace(clean)
	v = strings.TrimPrefix(v, "<")
	v = strings.TrimSuffix(v, ">")
	if v = removeWSP(v); v == "" {
		return nil
	}
	return &v
}

func languageOf(headers []HeaderField) []string {
	raw, ok := headerLast(headers, "Content-Language")
	if !ok {
		return nil
	}
	clean, _ := stripComments(unfold(raw))
	var tags []string
	for _, tag := range strings.Split(clean, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

func locationOf(headers []HeaderField) *string {
	raw, ok := headerLast(headers, "Content-Location")
	if !ok {
		return nil
	}
	clean, _ := stripComments(unfold(raw))
	if v := removeWSP(clean); v != "" {
		return &v
	}
	return nil
}

// delimiterLine reports whether line is a boundary delimiter and whether
// it is the close delimiter ("--boundary--"). Trailing white space is
// permitted per RFC 2046.
func delimiterLine(line, delim []byte) (bool, bool) {
	line = trimLineEnding(line)
	if !bytes.HasPrefix(line, delim) {
		return false, false
	}
	rest := line[len(delim):]
	isClose := bytes.HasPrefix(rest, []byte("--"))
	if isClose {
		rest = rest[2:]
	}
	return len(bytes.TrimRight(rest, " \t")) == 0, isClose
}

// trimPartTail removes the line ending that precedes the next delimiter;
// it belongs to the delimiter, not the part (RFC 2046 5.1.1).
func trimPartTail(part []byte) []byte {
	part = bytes.TrimSuffix(part, []byte("\n"))
	return bytes.TrimSuffix(part, []byte("\r"))
}
