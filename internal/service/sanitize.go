package service

import "strings"

// SanitizeLog removes terminal control sequences from CI log output while
// keeping newlines, tabs and SGR color sequences.
func SanitizeLog(input string) string {
	var out strings.Builder
	for i := 0; i < len(input); i++ {
		c := input[i]
		switch {
		case c == '\n' || c == '\t':
			out.WriteByte(c)
		case c == 0x1b:
			if i+1 < len(input) && input[i+1] == ']' {
				// OSC sequence: skip until BEL or ST.
				j := i + 2
				for j < len(input) {
					if input[j] == 0x07 {
						j++
						break
					}
					if input[j] == 0x1b && j+1 < len(input) && input[j+1] == '\\' {
						j += 2
						break
					}
					j++
				}
				i = j - 1
				continue
			}
			if i+1 < len(input) && input[i+1] == '[' {
				j := i + 2
				for j < len(input) {
					b := input[j]
					if (b >= '0' && b <= '9') || b == ';' || b == '?' {
						j++
						continue
					}
					break
				}
				if j < len(input) && input[j] >= '@' && input[j] <= '~' {
					if input[j] == 'm' {
						out.WriteString(input[i : j+1])
					}
					i = j
				}
			}
		case c < 0x20 || c == 0x7f:
			// Drop other C0 controls and DEL.
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}
