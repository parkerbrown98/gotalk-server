package api

import (
	"path"
	"reflect"
	"strings"
	"unicode"
)

// schemaNamer names OpenAPI schemas like huma.DefaultSchemaNamer, but qualifies types
// that share a name across packages (storage.Settings and mail.Settings become
// StorageSettings and MailSettings), including inside generic type names.
func schemaNamer(t reflect.Type, hint string) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	name := t.Name()
	if name == "" {
		name = hint
	}
	if name == "Settings" && t.PkgPath() != "" {
		name = path.Base(t.PkgPath()) + ".Settings"
	}
	name = strings.ReplaceAll(name, "[]", "List[")
	var b strings.Builder
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '[' || r == ']' || r == '*' || r == ',' }) {
		pkgPath, base := "", part
		if i := strings.LastIndex(part, "."); i >= 0 {
			pkgPath, base = part[:i], part[i+1:]
		}
		if base == "Settings" && pkgPath != "" {
			b.WriteString(upperFirst(path.Base(pkgPath)))
		}
		b.WriteString(upperFirst(base))
	}
	return b.String()
}

func upperFirst(s string) string {
	for i, r := range s {
		return string(unicode.ToUpper(r)) + s[i+len(string(r)):]
	}
	return s
}
