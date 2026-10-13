package license

// Protocol v1 fixed code point tables, copied from the vendor issuer
// (FORBIDDEN_TEXT_RANGES, WHITESPACE_RANGES) so both sides agree without
// depending on a Unicode database version.
var forbiddenTextRanges = [][2]rune{
	{0x0000, 0x001F}, {0x007F, 0x009F}, {0x00AD, 0x00AD}, {0x0600, 0x0605},
	{0x061C, 0x061C}, {0x06DD, 0x06DD}, {0x070F, 0x070F}, {0x0890, 0x0891},
	{0x08E2, 0x08E2}, {0x180E, 0x180E}, {0x200B, 0x200F}, {0x202A, 0x202E},
	{0x2060, 0x2064}, {0x2066, 0x206F}, {0xD800, 0xDFFF}, {0xE000, 0xF8FF},
	{0xFDD0, 0xFDEF}, {0xFEFF, 0xFEFF}, {0xFFF9, 0xFFFB}, {0x110BD, 0x110BD},
	{0x110CD, 0x110CD}, {0x13430, 0x1343F}, {0x1BCA0, 0x1BCA3},
	{0x1D173, 0x1D17A}, {0xE0001, 0xE0001}, {0xE0020, 0xE007F},
	{0xF0000, 0xFFFFD}, {0x100000, 0x10FFFD},
}

var whitespaceRanges = [][2]rune{
	{0x0009, 0x000D}, {0x0020, 0x0020}, {0x0085, 0x0085}, {0x00A0, 0x00A0},
	{0x1680, 0x1680}, {0x2000, 0x200A}, {0x2028, 0x2029}, {0x202F, 0x202F},
	{0x205F, 0x205F}, {0x3000, 0x3000},
}

func inRanges(r rune, ranges [][2]rune) bool {
	for _, rg := range ranges {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	return false
}

// validText applies the protocol v1 text rule: non-empty, at most limit
// UTF-16 code units, no forbidden code point or noncharacter, and no fixed
// whitespace unless allowSpaces, which still forbids it at either end.
func validText(s string, limit int, allowSpaces bool) bool {
	if s == "" || utf16Len(s) > limit {
		return false
	}
	runes := []rune(s)
	for i, r := range runes {
		if inRanges(r, forbiddenTextRanges) || r&0xFFFF == 0xFFFE || r&0xFFFF == 0xFFFF {
			return false
		}
		if inRanges(r, whitespaceRanges) && (!allowSpaces || i == 0 || i == len(runes)-1) {
			return false
		}
	}
	return true
}
