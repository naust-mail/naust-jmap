package oracle

import "strings"

// Flatten decomposes a bodyStructure tree into the textBody, htmlBody, and
// attachments lists. It is a direct port of the suggested algorithm in
// RFC 8621 4.1.4 (nil slice pointers play the JS nulls; pushes are
// nil-guarded where the original would throw on pathological nesting). It reads
// only metadata, so it operates on the parser's content-free tree.
func Flatten(root *Part) (textBody, htmlBody, attachments []*Part) {
	var tb, hb, ab []*Part
	tbp, hbp, abp := &tb, &hb, &ab
	flattenParts([]*Part{root}, "mixed", false, hbp, tbp, abp)
	return tb, hb, ab
}

func isInlineMediaType(typ string) bool {
	return strings.HasPrefix(typ, "image/") ||
		strings.HasPrefix(typ, "audio/") ||
		strings.HasPrefix(typ, "video/")
}

func flattenParts(parts []*Part, multipartType string, inAlternative bool, htmlBody, textBody, attachments *[]*Part) {
	textLength, htmlLength := -1, -1
	if textBody != nil {
		textLength = len(*textBody)
	}
	if htmlBody != nil {
		htmlLength = len(*htmlBody)
	}
	push := func(list *[]*Part, p *Part) {
		if list != nil {
			*list = append(*list, p)
		}
	}
	for i, part := range parts {
		isMultipart := strings.HasPrefix(part.Type, "multipart/")
		hasName := part.Name != nil && *part.Name != ""
		isInline := (part.Disposition == nil || *part.Disposition != "attachment") &&
			// Must be one of the allowed body types
			(part.Type == "text/plain" || part.Type == "text/html" || isInlineMediaType(part.Type)) &&
			// If multipart/related, only the first part can be inline.
			// If a text part with a filename, and not the first item in
			// the multipart, assume it is an attachment.
			(i == 0 || (multipartType != "related" && (isInlineMediaType(part.Type) || !hasName)))
		switch {
		case isMultipart:
			subMultiType := strings.TrimPrefix(part.Type, "multipart/")
			flattenParts(part.SubParts, subMultiType,
				inAlternative || subMultiType == "alternative",
				htmlBody, textBody, attachments)
		case isInline:
			if multipartType == "alternative" {
				switch part.Type {
				case "text/plain":
					push(textBody, part)
				case "text/html":
					push(htmlBody, part)
				default:
					push(attachments, part)
				}
				continue
			}
			if inAlternative {
				if part.Type == "text/plain" {
					htmlBody = nil
				}
				if part.Type == "text/html" {
					textBody = nil
				}
			}
			push(textBody, part)
			push(htmlBody, part)
			if (textBody == nil || htmlBody == nil) && isInlineMediaType(part.Type) {
				push(attachments, part)
			}
		default:
			push(attachments, part)
		}
	}
	if multipartType == "alternative" && textBody != nil && htmlBody != nil {
		// Found HTML part only
		if textLength == len(*textBody) && htmlLength != len(*htmlBody) {
			*textBody = append(*textBody, (*htmlBody)[htmlLength:]...)
		}
		// Found plaintext part only
		if htmlLength == len(*htmlBody) && textLength != len(*textBody) {
			*htmlBody = append(*htmlBody, (*textBody)[textLength:]...)
		}
	}
}

// HasAttachment implements the SHOULD rule of RFC 8621 4.1.4: true when
// the attachments list has at least one part that is not
// Content-Disposition: inline.
func HasAttachment(attachments []*Part) bool {
	for _, p := range attachments {
		if p.Disposition == nil || *p.Disposition != "inline" {
			return true
		}
	}
	return false
}
