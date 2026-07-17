package oracle

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/quotedprintable"
	"strconv"
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

// parsePart builds the bodyStructure node for one MIME entity.
// defaultType applies when the entity has no usable Content-Type
// ("text/plain", or "message/rfc822" inside multipart/digest, RFC 2046).
// counter numbers leaf parts depth-first for partId assignment; budget is
// the shared remaining-parts allowance that bounds the whole tree (maxParts).
// factory selects the sinks (if any) each leaf's content feeds; a multipart/*
// node has no content and never consults it.
func parsePart(headers []HeaderField, body []byte, defaultType string, counter, budget *int, depth int, factory SinkFactory) *Part {
	*budget--
	p := &Part{Headers: headers}
	typ, params, ctPresent := contentType(headers, defaultType)
	boundary := params["boundary"]
	// A multipart declares a boundary to separate its parts (RFC 2046 5.1.1);
	// with none, its body cannot be split, so it is a single part, represented as
	// the RFC 2046 default text/plain leaf rather than a childless multipart.
	if strings.HasPrefix(typ, "multipart/") && boundary == "" {
		typ, params, ctPresent = "text/plain", nil, true
	}
	p.Type = typ
	p.Charset = charsetOf(typ, params, ctPresent)
	p.Disposition, p.Name = dispositionOf(headers, params)
	p.Cid = angleValue(headers, "Content-Id")
	p.Language = languageOf(headers)
	p.Location = locationOf(headers)

	if strings.HasPrefix(typ, "multipart/") {
		p.SubParts = []*Part{} // non-nil: partId/blobId are null iff multipart/*
		if depth >= maxMultipartDepth {
			return p // too deeply nested to split; keep the type with no children
		}
		childDefault := "text/plain"
		if typ == "multipart/digest" {
			childDefault = "message/rfc822"
		}
		for _, sub := range splitMultipart(body, boundary, maxParts) {
			if *budget <= 0 {
				break // part budget exhausted (maxParts); drop the rest
			}
			subHeaders, subBody := parseHeaderBlock(sub)
			p.SubParts = append(p.SubParts, parsePart(subHeaders, subBody, childDefault, counter, budget, depth+1, factory))
		}
		return p
	}

	// Leaf part (message/rfc822 included: bodyStructure does not recurse
	// into embedded messages, RFC 8621 4.1.4). Content is decoded only for the
	// sinks the factory declares for this leaf; a leaf no consumer asked about
	// keeps its metadata and is never read.
	*counter++
	p.PartID = strconv.Itoa(*counter)
	feedLeafContent(p, body, cteOf(headers), factory)
	return p
}

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

// splitMultipart splits a multipart body at its boundary delimiters
// (RFC 2046 5.1.1). The preamble, the epilogue, and everything after the
// close delimiter are discarded; a missing close delimiter is tolerated.
// splitMultipart returns the raw segments between boundary delimiters. It
// stops after max segments so a body that is nothing but delimiters cannot
// allocate an unbounded slice (the caller enforces the same cap on the parts
// it builds); the surplus is dropped, matching maxParts truncation.
func splitMultipart(body []byte, boundary string, max int) [][]byte {
	delim := []byte("--" + boundary)
	var parts [][]byte
	start := -1 // offset of the current part's first byte; -1 = in preamble
	pos := 0
	for pos < len(body) {
		line, next := nextLine(body, pos)
		if isDelim, isClose := delimiterLine(line, delim); isDelim {
			if start >= 0 {
				parts = append(parts, trimPartTail(body[start:pos]))
			}
			if isClose || len(parts) >= max {
				return parts
			}
			start = next
		}
		pos = next
	}
	if start >= 0 && len(parts) < max {
		parts = append(parts, trimPartTail(body[start:]))
	}
	return parts
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

// decodeCTE removes the Content-Transfer-Encoding. Unknown encodings are
// treated as identity (RFC 8621 4.1.4) with the problem flag set, feeding
// EmailBodyValue.isEncodingProblem; decoding damage is likewise flagged.
func decodeCTE(body []byte, cte string) ([]byte, bool) {
	switch cte {
	case "", "7bit", "8bit", "binary":
		return body, false
	case "base64":
		return decodeBase64(body)
	case "quoted-printable":
		out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		return out, err != nil
	default:
		return body, true
	}
}

// decodeBase64 decodes leniently: white space is expected between lines;
// any other foreign octet is dropped and flagged as a problem. Padding marks
// the end of the encoded data (RFC 2045 6.8): base64 characters after it are
// trailing content, dropped and flagged rather than decoded.
func decodeBase64(body []byte) ([]byte, bool) {
	filtered := make([]byte, 0, len(body))
	problem := false
	padded := false
	for _, c := range body {
		switch {
		case c == '=':
			padded = true
		case c == ' ', c == '\t', c == '\r', c == '\n':
			// folding white space: ignored
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
			if padded {
				problem = true // base64 data after the padding: malformed
				continue
			}
			filtered = append(filtered, c)
		default:
			problem = true
		}
	}
	if n := len(filtered) % 4; n == 1 { // impossible base64 length
		filtered = filtered[:len(filtered)-1]
		problem = true
	}
	out, err := base64.RawStdEncoding.DecodeString(string(filtered))
	if err != nil {
		return nil, true
	}
	return out, problem
}
