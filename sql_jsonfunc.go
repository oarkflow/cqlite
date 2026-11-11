package cqlite

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// callJSONFunc implements SQLite's JSON scalar functions.
func callJSONFunc(name string, args []any) (any, bool, error) {
	switch name {
	case "json_valid":
		if len(args) != 1 {
			return nil, true, errors.New("json_valid expects 1 argument")
		}
		if args[0] == nil {
			return nil, true, nil
		}
		// json_valid() accepts only well-formed RFC 8259 JSON, even though
		// json() and json_error_position() tolerate SQLite's JSON5 extensions.
		if _, _, err := parseStrictJSON(stringifySQL(args[0])); err == nil {
			return int64(1), true, nil
		}
		return int64(0), true, nil
	case "json_error_position":
		if len(args) != 1 {
			return nil, true, errors.New("json_error_position expects 1 argument")
		}
		if args[0] == nil {
			return nil, true, nil
		}
		_, at, err := parseJSON5(stringifySQL(args[0]))
		if err != nil {
			return int64(at + 1), true, nil
		}
		return int64(0), true, nil
	case "json":
		if len(args) != 1 {
			return nil, true, errors.New("json expects 1 argument")
		}
		if args[0] == nil {
			return nil, true, nil
		}
		v, err := parseJSON(stringifySQL(args[0]))
		if err != nil {
			return nil, true, err
		}
		return encodeJSON(v), true, nil
	case "json_array":
		arr := make([]any, 0, len(args))
		for _, a := range args {
			arr = append(arr, jsonValueOf(a))
		}
		return encodeJSON(arr), true, nil
	case "json_object":
		if len(args)%2 != 0 {
			return nil, true, errors.New("json_object expects an even number of arguments")
		}
		obj := newJSONObject()
		for i := 0; i < len(args); i += 2 {
			if args[i] == nil {
				return nil, true, errors.New("json_object keys must not be NULL")
			}
			obj.set(stringifySQL(args[i]), jsonValueOf(args[i+1]))
		}
		return encodeJSON(obj), true, nil
	case "json_quote":
		if len(args) != 1 {
			return nil, true, errors.New("json_quote expects 1 argument")
		}
		if args[0] == nil {
			return "null", true, nil
		}
		if isIntegerValue(args[0]) {
			i, _ := asInt(args[0])
			return strconv.FormatInt(i, 10), true, nil
		}
		if f, ok := asFloat(args[0]); ok {
			return formatJSONNumber(f), true, nil
		}
		if s, ok := args[0].(string); ok {
			// A string that is already valid JSON is emitted as-is, matching
			// SQLite's json_quote behaviour for json() subvalues.
			return encodeJSON(s), true, nil
		}
		return encodeJSON(stringifySQL(args[0])), true, nil
	case "json_type":
		return jsonType(args)
	case "json_array_length":
		return jsonArrayLength(args)
	case "json_extract":
		return jsonExtract(args)
	case "json_insert":
		return jsonMutate(args, mutInsert)
	case "json_replace":
		return jsonMutate(args, mutReplace)
	case "json_set":
		return jsonMutate(args, mutSet)
	case "json_remove":
		return jsonRemove(args)
	case "json_patch":
		if len(args) != 2 {
			return nil, true, errors.New("json_patch expects 2 arguments")
		}
		if args[0] == nil || args[1] == nil {
			return nil, true, nil
		}
		x, err := parseJSON(stringifySQL(args[0]))
		if err != nil {
			return nil, true, err
		}
		y, err := parseJSON(stringifySQL(args[1]))
		if err != nil {
			return nil, true, err
		}

		return encodeJSON(mergePatch(x, y)), true, nil
	}
	return nil, false, nil
}

// parseJSON decodes JSON (including SQLite's accepted JSON5 extensions) while
// preserving the integer-ness of whole numbers.
func parseJSON(s string) (any, error) {
	v, _, err := parseJSON5(s)
	if err != nil {
		return nil, fmt.Errorf("malformed JSON: %w", err)
	}
	return normalizeJSON(v), nil
}

// jsonValueOf converts a SQL value into a JSON value. Text that is already
// valid JSON is embedded as JSON; otherwise it becomes a JSON string.
func jsonValueOf(v any) any {
	if v == nil {
		return nil
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	if isIntegerValue(v) {
		i, _ := asInt(v)
		return i
	}
	if f, ok := asFloat(v); ok {
		return f
	}
	s := stringifySQL(v)
	if parsed, err := parseJSON(s); err == nil {
		return parsed
	}
	return s
}

// encodeJSON renders a decoded value as compact JSON text with keys in
// insertion order.
func encodeJSON(v any) string {
	var b strings.Builder
	writeJSON(&b, v)
	return b.String()
}

func writeJSON(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		b.WriteString(formatJSONNumber(x))
	case string:
		writeJSONString(b, x)
	case []byte:
		writeJSONString(b, string(x))
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSON(b, e)
		}
		b.WriteByte(']')
	case *jsonObject:
		b.WriteByte('{')
		for i, k := range x.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONString(b, k)
			b.WriteByte(':')
			writeJSON(b, x.m[k])
		}
		b.WriteByte('}')
	case map[string]any:
		obj := newJSONObject()
		for k, v := range x {
			obj.set(k, v)
		}
		writeJSON(b, obj)
	default:
		writeJSONString(b, fmt.Sprint(x))
	}
}

func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\b':
			b.WriteString("\\b")
		case '\f':
			b.WriteString("\\f")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			if c < 0x20 {
				b.WriteString(fmt.Sprintf("\\u%04x", c))
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
}

func formatJSONNumber(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if s == "NaN" || s == "Inf" || s == "-Inf" {
		return "null"
	}
	return s
}

// jsonPath is a parsed JSON path starting at the '$' root.
type jsonPath struct {
	steps []jsonStep
}

type jsonStep struct {
	key  string
	idx  int
	isIx bool
}

// parseJSONPath parses SQLite's $.a.b[0] path syntax. The '#' terminator is
// accepted for json_remove's "whole document" form.
func parseJSONPath(p string) (jsonPath, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return jsonPath{}, errors.New("empty JSON path")
	}
	switch p[0] {
	case '$':
		p = p[1:]
	case '#':
		return jsonPath{}, nil
	}
	var out jsonPath
	for p != "" {
		switch p[0] {
		case '.':
			p = p[1:]
			start := 0
			for start < len(p) && p[start] != '.' && p[start] != '[' {
				start++
			}
			out.steps = append(out.steps, jsonStep{key: p[:start]})
			p = p[start:]
		case '[':
			p = p[1:]
			end := strings.IndexByte(p, ']')
			if end < 0 {
				return jsonPath{}, errors.New("unterminated JSON path index")
			}
			idxStr := p[:end]
			p = p[end+1:]
			if idxStr == "" {
				return jsonPath{}, errors.New("empty JSON path index")
			}
			ix, err := strconv.Atoi(idxStr)
			if err != nil {
				// [ "key" ] form
				key := strings.Trim(idxStr, `"'`)
				out.steps = append(out.steps, jsonStep{key: key})
				continue
			}
			out.steps = append(out.steps, jsonStep{idx: ix, isIx: true})
		default:
			return jsonPath{}, fmt.Errorf("unexpected character in JSON path: %q", p[0])
		}
	}
	return out, nil
}

func jsonType(args []any) (any, bool, error) {
	if len(args) < 1 || len(args) > 2 {
		return nil, true, errors.New("json_type expects 1 or 2 arguments")
	}
	if args[0] == nil {
		return nil, true, nil
	}
	v, err := parseJSON(stringifySQL(args[0]))
	if err != nil {
		return nil, true, err
	}
	if len(args) == 2 {
		path, err := parseJSONPath(stringifySQL(args[1]))
		if err != nil {
			return nil, true, err
		}
		found := false
		v, found = walkJSONPath(v, path)
		if !found {
			return nil, true, nil
		}
	}
	return jsonTypeName(v), true, nil
}

func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case int64, int:
		return "integer"
	case float64:
		return "real"
	case string:
		return "text"
	case []any:
		return "array"
	case *jsonObject, map[string]any:
		return "object"
	}
	return "text"
}

func jsonArrayLength(args []any) (any, bool, error) {
	if len(args) < 1 || len(args) > 2 {
		return nil, true, errors.New("json_array_length expects 1 or 2 arguments")
	}
	if args[0] == nil {
		return nil, true, nil
	}
	v, err := parseJSON(stringifySQL(args[0]))
	if err != nil {
		return nil, true, err
	}
	if len(args) == 2 {
		path, err := parseJSONPath(stringifySQL(args[1]))
		if err != nil {
			return nil, true, err
		}
		found := false
		v, found = walkJSONPath(v, path)
		if !found {
			return nil, true, nil
		}
	}
	arr, ok := v.([]any)
	if !ok {
		// SQLite reports length 0 for a non-array rather than failing.
		return int64(0), true, nil
	}
	return int64(len(arr)), true, nil
}

func jsonExtract(args []any) (any, bool, error) {
	if len(args) < 2 {
		return nil, true, errors.New("json_extract expects at least 2 arguments")
	}
	if args[0] == nil {
		return nil, true, nil
	}
	v, err := parseJSON(stringifySQL(args[0]))
	if err != nil {
		return nil, true, err
	}
	var results []any
	for _, p := range args[1:] {
		path, err := parseJSONPath(stringifySQL(p))
		if err != nil {
			return nil, true, err
		}
		found := false
		val, f := walkJSONPath(v, path)
		found = f
		if !found {
			results = append(results, nil)
			continue
		}
		results = append(results, val)
	}
	if len(results) == 1 {
		return jsonScalar(results[0]), true, nil
	}
	// Multiple paths produce a JSON array of the extracted values.
	return encodeJSON(results), true, nil
}

// jsonScalar converts a JSON value into a SQL value. Objects and arrays become
// compact JSON text; scalars become native SQL values.
func jsonScalar(v any) any {
	switch x := v.(type) {
	case []any:
		return encodeJSON(x)
	case *jsonObject, map[string]any:
		return encodeJSON(x)
	default:
		return x
	}
}

func walkJSONPath(root any, path jsonPath) (any, bool) {
	cur := root
	for _, step := range path.steps {
		switch c := cur.(type) {
		case *jsonObject:
			val, ok := c.get(step.key)
			if !ok {
				return nil, false
			}
			cur = val
		case map[string]any:
			val, ok := c[step.key]
			if !ok {
				return nil, false
			}
			cur = val
		case []any:
			if !step.isIx {
				return nil, false
			}
			if step.idx < 0 || step.idx >= len(c) {
				return nil, false
			}
			cur = c[step.idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

type mutateMode int

const (
	mutInsert mutateMode = iota
	mutReplace
	mutSet
)

func jsonMutate(args []any, mode mutateMode) (any, bool, error) {
	if len(args) < 3 || len(args)%2 == 0 {
		return nil, true, errors.New("json mutation expects a document, path/value pairs")
	}
	if args[0] == nil {
		return nil, true, nil
	}
	root, err := parseJSON(stringifySQL(args[0]))
	if err != nil {
		return nil, true, err
	}
	for i := 1; i+1 < len(args); i += 2 {
		path, err := parseJSONPath(stringifySQL(args[i]))
		if err != nil {
			return nil, true, err
		}
		val := jsonValueOf(args[i+1])
		root = mutateJSON(root, path, val, mode)
	}
	return encodeJSON(root), true, nil
}

// mutateJSON applies json_insert/replace/set semantics at a path.
func mutateJSON(root any, path jsonPath, val any, mode mutateMode) any {
	if len(path.steps) == 0 {
		return val
	}
	return mutateAt(root, path.steps, val, mode)
}

func mutateAt(cur any, steps []jsonStep, val any, mode mutateMode) any {
	step := steps[0]
	last := len(steps) == 1
	if obj, isObj := cur.(*jsonObject); isObj {
		child, exists := obj.get(step.key)
		if last {
			switch {
			case exists && mode == mutInsert:
				return obj
			case !exists && mode == mutReplace:
				return obj
			default:
				obj.set(step.key, val)
				return obj
			}
		}
		if !exists {
			if mode == mutReplace {
				return obj
			}
			// Determine the container type from the next step.
			if steps[1].isIx {
				child = []any{}
			} else {
				child = newJSONObject()
			}
			obj.set(step.key, child)
		}
		obj.set(step.key, mutateAt(child, steps[1:], val, mode))
		return obj
	}
	if obj, ok := cur.(map[string]any); ok {
		child, exists := obj[step.key]
		if last {
			switch {
			case exists && mode == mutInsert:
				return obj
			case !exists && mode == mutReplace:
				return obj
			default:
				obj[step.key] = val
				return obj
			}
		}
		if !exists {
			if mode == mutReplace {
				return obj
			}
			if steps[1].isIx {
				child = []any{}
			} else {
				child = newJSONObject()
			}
			obj[step.key] = child
		}
		obj[step.key] = mutateAt(child, steps[1:], val, mode)
		return obj
	}
	if arr, ok := cur.([]any); ok {
		idx := step.idx
		if idx < 0 {
			idx += len(arr)
		}
		if last {
			if idx >= 0 && idx < len(arr) {
				if mode == mutInsert {
					return arr
				}
				arr[idx] = val
				return arr
			}
			if mode == mutReplace || idx < 0 {
				return arr
			}
			if idx > len(arr) {
				idx = len(arr)
			}
			out := make([]any, 0, len(arr)+1)
			out = append(out, arr[:idx]...)
			out = append(out, val)
			out = append(out, arr[idx:]...)
			return out
		}
		if idx >= 0 && idx < len(arr) {
			arr[idx] = mutateAt(arr[idx], steps[1:], val, mode)
			return arr
		}
		if mode == mutReplace || idx < 0 {
			return arr
		}
		if idx > len(arr) {
			idx = len(arr)
		}
		var child any
		if steps[1].isIx {
			child = []any{}
		} else {
			child = newJSONObject()
		}
		child = mutateAt(child, steps[1:], val, mode)
		out := make([]any, 0, len(arr)+1)
		out = append(out, arr[:idx]...)
		out = append(out, child)
		out = append(out, arr[idx:]...)
		return out
	}
	// Scalar in the way: create a container and retry.
	var container any
	if step.isIx {
		container = []any{}
	} else {
		container = newJSONObject()
	}
	return mutateAt(container, steps, val, mode)
}

func jsonRemove(args []any) (any, bool, error) {
	if len(args) < 2 {
		return nil, true, errors.New("json_remove expects at least 2 arguments")
	}
	if args[0] == nil {
		return nil, true, nil
	}
	root, err := parseJSON(stringifySQL(args[0]))
	if err != nil {
		return nil, true, err
	}
	// Paths are applied in order, and the document may change between them.
	for _, p := range args[1:] {
		path, err := parseJSONPath(stringifySQL(p))
		if err != nil {
			return nil, true, err
		}
		if len(path.steps) == 0 {
			continue
		}
		root = removeJSON(root, path.steps)
	}
	return encodeJSON(root), true, nil
}

func removeJSON(cur any, steps []jsonStep) any {
	step := steps[0]
	last := len(steps) == 1
	if obj, ok := cur.(*jsonObject); ok {
		child, exists := obj.get(step.key)
		if !exists {
			return obj
		}
		if last {
			obj.delete(step.key)
			return obj
		}
		obj.set(step.key, removeJSON(child, steps[1:]))
		return obj
	}
	if arr, ok := cur.([]any); ok {
		idx := step.idx
		if idx < 0 {
			idx += len(arr)
		}
		if idx < 0 || idx >= len(arr) {
			return arr
		}
		if last {
			out := make([]any, 0, len(arr)-1)
			out = append(out, arr[:idx]...)
			out = append(out, arr[idx+1:]...)
			return out
		}
		arr[idx] = removeJSON(arr[idx], steps[1:])
		return arr
	}
	return cur
}

// mergePatch implements RFC 7386 JSON Merge Patch over ordered objects.
func mergePatch(target, patch any) any {
	pobj, ok := patch.(*jsonObject)
	if !ok {
		return patch
	}
	tobj, ok := target.(*jsonObject)
	if !ok {
		tobj = newJSONObject()
	}
	for _, k := range pobj.keys {
		v := pobj.m[k]
		if v == nil {
			tobj.delete(k)
			continue
		}
		if pm, ok := v.(*jsonObject); ok {
			tv, _ := tobj.get(k)
			tobj.set(k, mergePatch(tv, pm))
			continue
		}
		tobj.set(k, v)
	}
	return tobj
}
