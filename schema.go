// schema.go — my form schema types and validation.
//
// The form schema is a plain JSON document my builder serializes and I
// persist in the `schema` JSON field of the `forms` collection. The
// canonical form title is deliberately NOT part of this document — it lives
// on the form record itself, so I can rename a form without re-serializing
// the schema (writing this down now so Stage 3 me doesn't create a second,
// "real" title somewhere else).
package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// My question types. I keep "choice" (radio group) and "dropdown" (<select>)
// as separate types because my results panel charts them differently — the
// same split is documented in pb_public/js/app.js.
const (
	questionTypeText     = "text"
	questionTypeChoice   = "choice"
	questionTypeDropdown = "dropdown"
)

// questionIdPattern — my builder generates ids as q1..qN, but I validate on
// the server anyway because these strings become SQLite column names on the
// dynamic collection. Only a conservative lowercase identifier is accepted,
// so a hostile (or buggy) payload can never inject SQL or break my field
// mapping.
var questionIdPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)

// formTitleMaxChars and textAnswerMaxChars are my length promises in
// CHARACTERS (runes), not bytes. My handlers enforce them in runes (see
// validateSchema and my title checks), and the underlying columns carry
// the same numbers as their Max — PocketBase counts Max in runes too
// (v0.40.6 field_text.go: "casted to []rune to count multi-byte chars as
// one"), so handler and column agree on one unit. See
// ensureFormsCollection and buildDynamicCollection.
const (
	formTitleMaxChars  = 200
	textAnswerMaxChars = 2000
)

// questionSpec is one question of my form schema. It mirrors the JS shape
// in app.js exactly — same JSON, no translation layer.
type questionSpec struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Label    string   `json:"label"`
	Required bool     `json:"required"`
	Options  []string `json:"options,omitempty"` // choice/dropdown only
}

// formSchema is the JSON document I persist per form.
type formSchema struct {
	Questions []questionSpec `json:"questions"`
}

// validateSchema checks a submitted schema before I let it create a dynamic
// collection. I run it on form create; Stage 3 reuses it when co-creators
// gain schema editing.
func validateSchema(s formSchema) error {
	if len(s.Questions) == 0 {
		return errors.New("a form needs at least one question")
	}
	if len(s.Questions) > 50 {
		return errors.New("a form can have at most 50 questions")
	}

	seen := make(map[string]bool, len(s.Questions))
	for i, q := range s.Questions {
		if !questionIdPattern.MatchString(q.ID) {
			return fmt.Errorf("question %d: invalid id %q", i+1, q.ID)
		}
		if seen[q.ID] {
			return fmt.Errorf("question %d: duplicate id %q", i+1, q.ID)
		}
		seen[q.ID] = true

		// Trim before checking (audit #9): a spaces-only label is blank
		// for every respondent, so it fails the same way an empty one
		// does. (Options get the same trim a few lines below.)
		label := strings.TrimSpace(q.Label)
		// Character (rune) count on purpose — len() counts BYTES, which
		// would reject a perfectly valid 200-character CJK label (~600
		// bytes) while the frontend maxlength is character-based.
		if label == "" || utf8.RuneCountInString(label) > 200 {
			return fmt.Errorf("question %d: label must be 1-200 characters", i+1)
		}

		switch q.Type {
		case questionTypeText:
			// no options — any provided would be ignored
		case questionTypeChoice, questionTypeDropdown:
			if len(q.Options) < 2 {
				return fmt.Errorf("question %d: %s questions need at least 2 options", i+1, q.Type)
			}
			if len(q.Options) > 20 {
				return fmt.Errorf("question %d: %s questions allow at most 20 options", i+1, q.Type)
			}
			optSeen := make(map[string]bool, len(q.Options))
			for _, opt := range q.Options {
				opt = strings.TrimSpace(opt)
				if opt == "" {
					return fmt.Errorf("question %d: empty option not allowed", i+1)
				}
				if utf8.RuneCountInString(opt) > 100 {
					return fmt.Errorf("question %d: options must be 100 characters or less", i+1)
				}
				if optSeen[opt] {
					return fmt.Errorf("question %d: duplicate option %q", i+1, opt)
				}
				optSeen[opt] = true
			}
		default:
			return fmt.Errorf("question %d: unknown type %q", i+1, q.Type)
		}
	}

	return nil
}
