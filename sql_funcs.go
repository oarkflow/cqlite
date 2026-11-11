package db

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"unicode/utf8"
)

// callExtendedFunc implements SQLite's scalar built-ins beyond the small set
// historically supported by the engine. Return types follow SQLite: functions
// that are inherently fractional (round, mod, power, floor, ...) return real,
// while functions that preserve integer-ness (abs, sign, length) return
// integer when given integers.
func callExtendedFunc(name string, args []any) (any, error) {
	switch name {
	case "substr", "substring":
		return fnSubstr(args)
	case "trim":
		return fnTrim(args, true, true)
	case "ltrim":
		return fnTrim(args, true, false)
	case "rtrim":
		return fnTrim(args, false, true)
	case "replace":
		return fnReplace(args)
	case "instr":
		return fnInstr(args)
	case "printf", "format":
		return fnPrintf(args)
	case "quote":
		if len(args) != 1 {
			return nil, errors.New("quote expects 1 argument")
		}
		return sqlQuote(args[0]), nil
	case "char":
		return fnChar(args)
	case "unicode":
		if len(args) != 1 {
			return nil, errors.New("unicode expects 1 argument")
		}
		if args[0] == nil {
			return nil, nil
		}
		s := stringifySQL(args[0])
		if s == "" {
			return nil, nil
		}
		r, _ := utf8.DecodeRuneInString(s)
		return int64(r), nil
	case "unhex":
		return fnUnhex(args)
	case "zeroblob":
		n, err := argInt(args, 0, "zeroblob")
		if err != nil {
			return nil, err
		}
		if n < 0 {
			n = 0
		}
		return make([]byte, n), nil
	case "randomblob":
		n, err := argInt(args, 0, "randomblob")
		if err != nil {
			return nil, err
		}
		if n < 0 {
			n = 0
		}
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rand.Intn(256))
		}
		return b, nil
	case "random":
		return rand.Int63(), nil
	case "iif":
		if len(args) != 3 {
			return nil, errors.New("iif expects 3 arguments")
		}
		if truthy(args[0]) {
			return args[1], nil
		}
		return args[2], nil
	case "min", "max":
		return fnMinMax(name, args)
	case "sign":
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		f, ok := asFloat(args[0])
		if !ok {
			return nil, nil
		}
		switch {
		case f < 0:
			return int64(-1), nil
		case f > 0:
			return int64(1), nil
		default:
			return int64(0), nil
		}
	case "round":
		return fnRound(args)
	case "abs":
		if len(args) != 1 || args[0] == nil {
			return nil, nil
		}
		if isIntegerValue(args[0]) {
			i, _ := asInt(args[0])
			if i == math.MinInt64 {
				return nil, errors.New("integer overflow")
			}
			if i < 0 {
				return -i, nil
			}
			return i, nil
		}
		f, ok := asFloat(args[0])
		if !ok {
			return nil, nil
		}
		return math.Abs(f), nil
	case "octet_length", "lengthb":
		if len(args) != 1 {
			return nil, errors.New("octet_length expects 1 argument")
		}
		if args[0] == nil {
			return nil, nil
		}
		return int64(len(stringifySQL(args[0]))), nil
	case "likelihood":
		// likelihood(X, Y) is a query-planner hint that evaluates to X.
		if len(args) != 2 {
			return nil, errors.New("likelihood expects 2 arguments")
		}
		return args[0], nil
	case "likely", "unlikely":
		if len(args) != 1 {
			return nil, fmt.Errorf("%s expects 1 argument", name)
		}
		return args[0], nil
	case "concat":
		var b strings.Builder
		for _, a := range args {
			if a == nil {
				continue
			}
			b.WriteString(stringifySQL(a))
		}
		return b.String(), nil
	case "concat_ws":
		if len(args) < 1 {
			return nil, errors.New("concat_ws expects at least 1 argument")
		}
		if args[0] == nil {
			return nil, nil
		}
		sep := stringifySQL(args[0])
		var parts []string
		for _, a := range args[1:] {
			if a == nil {
				continue
			}
			parts = append(parts, stringifySQL(a))
		}
		return strings.Join(parts, sep), nil
	case "pi":
		return math.Pi, nil
	case "pow", "power":
		return fnFloat2(args, name, math.Pow)
	case "sqrt":
		return fnFloat1(args, name, math.Sqrt)
	case "exp":
		return fnFloat1(args, name, math.Exp)
	case "ln", "log":
		if len(args) == 2 {
			// log(B, X) is log base B of X.
			return fnFloat2(args, name, func(b, x float64) float64 {
				if b <= 0 || b == 1 || x <= 0 {
					return math.NaN()
				}
				return math.Log(x) / math.Log(b)
			})
		}
		return fnFloat1(args, name, math.Log)
	case "log2":
		return fnFloat1(args, name, math.Log2)
	case "log10":
		return fnFloat1(args, name, math.Log10)
	case "floor":
		return fnFloat1(args, name, math.Floor)
	case "ceil", "ceiling":
		return fnFloat1(args, name, math.Ceil)
	case "trunc":
		return fnFloat1(args, name, math.Trunc)
	case "sin":
		return fnFloat1(args, name, math.Sin)
	case "cos":
		return fnFloat1(args, name, math.Cos)
	case "tan":
		return fnFloat1(args, name, math.Tan)
	case "sinh":
		return fnFloat1(args, name, math.Sinh)
	case "cosh":
		return fnFloat1(args, name, math.Cosh)
	case "tanh":
		return fnFloat1(args, name, math.Tanh)
	case "asin":
		return fnFloat1(args, name, math.Asin)
	case "acos":
		return fnFloat1(args, name, math.Acos)
	case "atan":
		return fnFloat1(args, name, math.Atan)
	case "asinh":
		return fnFloat1(args, name, math.Asinh)
	case "acosh":
		return fnFloat1(args, name, math.Acosh)
	case "atanh":
		return fnFloat1(args, name, math.Atanh)
	case "atan2":
		return fnFloat2(args, name, math.Atan2)
	case "radians":
		return fnFloat1(args, name, func(v float64) float64 { return v * math.Pi / 180 })
	case "degrees":
		return fnFloat1(args, name, func(v float64) float64 { return v * 180 / math.Pi })
	case "mod":
		return fnMod(args)
	case "like":
		return fnLike(args)
	case "glob":
		return fnGlob(args)
	case "raise":
		return fnRaise(args)
	}
	// Date/time and JSON families live in their own files to keep this
	// dispatch table readable.
	if v, ok, err := callTimeFunc(name, args); ok {
		return v, err
	}
	if v, ok, err := callJSONFunc(name, args); ok {
		return v, err
	}
	return nil, fmt.Errorf("unsupported function: %s", name)
}

// fnRaise implements SQLite's RAISE() function. It is valid only inside a
// trigger program and raises a control-flow signal rather than a value:
// RAISE(IGNORE) with no message, or RAISE(level, message) with both.
func fnRaise(args []any) (any, error) {
	if activeTriggerRows == nil {
		return nil, errors.New("RAISE() may only be used within a trigger-program")
	}
	if len(args) == 1 {
		action := strings.ToLower(stringifySQL(args[0]))
		switch action {
		case "ignore":
			return nil, raiseSignal("ignore", "")
		case "rollback", "abort", "fail":
			return nil, fmt.Errorf("RAISE(%s) requires an error message", strings.ToUpper(action))
		}
		return nil, fmt.Errorf("unknown RAISE action: %s", action)
	}
	if len(args) == 2 {
		action := strings.ToLower(stringifySQL(args[0]))
		switch action {
		case "ignore":
			return nil, errors.New("RAISE(IGNORE) takes no error message")
		case "rollback", "abort", "fail":
			return nil, raiseSignal(action, stringifySQL(args[1]))
		}
		return nil, fmt.Errorf("unknown RAISE action: %s", action)
	}
	return nil, errors.New("RAISE expects 1 or 2 arguments")
}

func argFloat(args []any, i int, fn string) (float64, error) {
	if i >= len(args) {
		return 0, fmt.Errorf("%s expects more arguments", fn)
	}
	if args[i] == nil {
		return 0, nil
	}
	f, ok := asFloat(args[i])
	if !ok {
		return 0, fmt.Errorf("%s expects numeric arguments", fn)
	}
	return f, nil
}

func argInt(args []any, i int, fn string) (int, error) {
	if i >= len(args) {
		return 0, fmt.Errorf("%s expects more arguments", fn)
	}
	if args[i] == nil {
		return 0, nil
	}
	f, ok := asFloat(args[i])
	if !ok {
		return 0, fmt.Errorf("%s expects numeric arguments", fn)
	}
	return int(f), nil
}

func argText(args []any, i int, fn string) (string, error) {
	if i >= len(args) {
		return "", fmt.Errorf("%s expects more arguments", fn)
	}
	if args[i] == nil {
		return "", nil
	}
	return stringifySQL(args[i]), nil
}

// fnFloat1 wraps a unary float function. SQLite returns real for the whole
// math family, and NULL when the argument is NULL or not numeric.
func fnFloat1(args []any, name string, f func(float64) float64) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("%s expects 1 argument", name)
	}
	if args[0] == nil {
		return nil, nil
	}
	v, ok := asFloat(args[0])
	if !ok {
		return nil, fmt.Errorf("%s expects numeric arguments", name)
	}
	return f(v), nil
}

func fnFloat2(args []any, name string, f func(float64, float64) float64) (any, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("%s expects 2 arguments", name)
	}
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	a, ok := asFloat(args[0])
	if !ok {
		return nil, fmt.Errorf("%s expects numeric arguments", name)
	}
	b, ok := asFloat(args[1])
	if !ok {
		return nil, fmt.Errorf("%s expects numeric arguments", name)
	}
	return f(a, b), nil
}

// fnSubstr implements substr(X,Y[,Z]) with SQLite's 1-based, negative-aware
// indexing. substr(X,Y,-N) returns the N characters that precede position Y.
func fnSubstr(args []any) (any, error) {
	if len(args) < 2 || len(args) > 3 {
		return nil, errors.New("substr expects 2 or 3 arguments")
	}
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	if len(args) == 3 && args[2] == nil {
		return nil, nil
	}
	blob, isBlob := args[0].([]byte)
	s := stringifySQL(args[0])
	runes := []rune(s)
	if isBlob {
		// BLOBs index by byte, not rune.
		start, ok := asInt(args[1])
		if !ok {
			return nil, errors.New("substr expects integer position")
		}
		n := len(blob)
		var count int64 = int64(n)
		hasCount := len(args) == 3
		if hasCount {
			c, ok := asInt(args[2])
			if !ok {
				return nil, errors.New("substr expects integer length")
			}
			count = c
		}
		pos := int(start)
		if start < 0 {
			pos = n + int(start) + 1
		}
		if count < 0 {
			end := pos
			pos = pos + int(count)
			count = int64(end - pos)
		}
		if pos < 1 {
			count -= int64(1 - pos)
			pos = 1
		}
		if pos > n || count <= 0 {
			return []byte{}, nil
		}
		end := pos - 1 + int(count)
		if end > n {
			end = n
		}
		out := make([]byte, end-pos+1)
		copy(out, blob[pos-1:end])
		return out, nil
	}
	n := len(runes)
	start, ok := asInt(args[1])
	if !ok {
		return nil, errors.New("substr expects integer position")
	}
	var count int64 = int64(n)
	hasCount := len(args) == 3
	if hasCount {
		c, ok := asInt(args[2])
		if !ok {
			return nil, errors.New("substr expects integer length")
		}
		count = c
	}
	pos := int(start)
	if start < 0 {
		pos = n + int(start) + 1
	}
	if count < 0 {
		end := pos
		pos = pos + int(count)
		count = int64(end - pos)
	}
	if pos < 1 {
		count -= int64(1 - pos)
		pos = 1
	}
	if pos > n || count <= 0 {
		return "", nil
	}
	end := pos - 1 + int(count)
	if end > n {
		end = n
	}
	return string(runes[pos-1 : end]), nil
}

// fnTrim implements trim/ltrim/rtrim with an optional character set to strip.
func fnTrim(args []any, left, right bool) (any, error) {
	if len(args) < 1 || len(args) > 2 {
		return nil, errors.New("trim expects 1 or 2 arguments")
	}
	if args[0] == nil {
		return nil, nil
	}
	s := stringifySQL(args[0])
	cut := " "
	if len(args) == 2 {
		if args[1] == nil {
			return nil, nil
		}
		cut = stringifySQL(args[1])
	}
	if cut == "" {
		return s, nil
	}
	if left {
		s = strings.TrimLeft(s, cut)
	}
	if right {
		s = strings.TrimRight(s, cut)
	}
	return s, nil
}

func fnReplace(args []any) (any, error) {
	if len(args) != 3 {
		return nil, errors.New("replace expects 3 arguments")
	}
	if args[0] == nil {
		return nil, nil
	}
	s := stringifySQL(args[0])
	if args[1] == nil {
		return s, nil
	}
	old := stringifySQL(args[1])
	if old == "" {
		return s, nil
	}
	var repl string
	if args[2] != nil {
		repl = stringifySQL(args[2])
	}
	return strings.ReplaceAll(s, old, repl), nil
}

func fnInstr(args []any) (any, error) {
	if len(args) != 2 {
		return nil, errors.New("instr expects 2 arguments")
	}
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	x, y := stringifySQL(args[0]), stringifySQL(args[1])
	if x1, ok := args[0].([]byte); ok {
		y1, _ := args[1].([]byte)
		idx := strings.Index(string(x1), string(y1))
		if idx < 0 {
			return int64(0), nil
		}
		return int64(idx + 1), nil
	}
	idx := strings.Index(x, y)
	if idx < 0 {
		return int64(0), nil
	}
	return int64(utf8.RuneCountInString(x[:idx]) + 1), nil
}

// fnPrintf supports SQLite's %d %i %s %f %g %e %c %x %o %z %q %Q %% formats.
func fnPrintf(args []any) (any, error) {
	if len(args) < 1 {
		return nil, errors.New("printf expects at least 1 argument")
	}
	format, err := argText(args, 0, "printf")
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	rest := args[1:]
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 >= len(format) {
			b.WriteByte(format[i])
			continue
		}
		i++
		// flags and width/precision
		start := i
		for i < len(format) && strings.ContainsRune("-+ #0", rune(format[i])) {
			i++
		}
		flags := format[start:i]
		width := ""
		for i < len(format) && format[i] >= '0' && format[i] <= '9' {
			width += string(format[i])
			i++
		}
		prec := ""
		if i < len(format) && format[i] == '.' {
			i++
			for i < len(format) && format[i] >= '0' && format[i] <= '9' {
				prec += string(format[i])
				i++
			}
		}
		if i >= len(format) {
			b.WriteByte('%')
			break
		}
		verb := format[i]
		if verb == '%' {
			b.WriteByte('%')
			continue
		}
		var arg any
		if len(rest) > 0 {
			arg, rest = rest[0], rest[1:]
		}
		spec := "%" + flags
		if width != "" {
			spec += width
		}
		if prec != "" {
			spec += "." + prec
		}
		switch verb {
		case 'd', 'i':
			v, _ := asInt(arg)
			b.WriteString(fmt.Sprintf(spec+"d", v))
		case 'u':
			v, _ := asInt(arg)
			b.WriteString(fmt.Sprintf(spec+"d", v))
		case 'f', 'e', 'g', 'G', 'E':
			v, _ := asFloat(arg)
			b.WriteString(fmt.Sprintf(spec+string(verb), v))
		case 'c':
			v, _ := asInt(arg)
			b.WriteRune(rune(v))
		case 's':
			b.WriteString(fmt.Sprintf(spec+"s", stringifySQL(arg)))
		case 'x':
			v, _ := asInt(arg)
			b.WriteString(fmt.Sprintf(spec+"x", v))
		case 'X':
			v, _ := asInt(arg)
			b.WriteString(fmt.Sprintf(spec+"X", v))
		case 'o':
			v, _ := asInt(arg)
			b.WriteString(fmt.Sprintf(spec+"o", v))
		case 'q':
			b.WriteString(strings.ReplaceAll(stringifySQL(arg), "'", "''"))
		case 'Q':
			b.WriteString(sqlQuote(arg))
		case 'z':
			b.WriteString(fmt.Sprintf(spec+"s", stringifySQL(arg)))
		default:
			b.WriteByte('%')
			b.WriteByte(verb)
		}
	}
	return b.String(), nil
}

// sqlQuote renders a value as a SQL literal.
func sqlQuote(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if x {
			return "1"
		}
		return "0"
	case []byte:
		return "X'" + strings.ToUpper(hex.EncodeToString(x)) + "'"
	}
	if isIntegerValue(v) {
		i, _ := asInt(v)
		return strconv.FormatInt(i, 10)
	}
	if f, ok := asFloat(v); ok {
		// SQLite renders whole reals with a trailing .0 so they stay REAL.
		s := strconv.FormatFloat(f, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		return s
	}
	s := stringifySQL(v)
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// fnChar implements char(X1,X2,...). SQLite stops at the first NULL argument
// rather than skipping it.
func fnChar(args []any) (any, error) {
	var b strings.Builder
	for _, a := range args {
		if a == nil {
			break
		}
		v, ok := asInt(a)
		if !ok {
			continue
		}
		b.WriteRune(rune(v))
	}
	return b.String(), nil
}

func fnUnhex(args []any) (any, error) {
	if len(args) < 1 || len(args) > 2 {
		return nil, errors.New("unhex expects 1 or 2 arguments")
	}
	if args[0] == nil {
		return nil, nil
	}
	s := stringifySQL(args[0])
	if len(s)%2 == 1 {
		if len(args) == 2 {
			s = stringifySQL(args[1]) + s
		} else {
			return nil, nil
		}
	}
	out, err := hex.DecodeString(s)
	if err != nil {
		return nil, nil
	}
	return out, nil
}

// fnMinMax implements the scalar multi-argument min()/max(). Unlike the
// aggregate form, a NULL argument makes the whole result NULL.
func fnMinMax(name string, args []any) (any, error) {
	var best any
	for _, a := range args {
		if a == nil {
			return nil, nil
		}
		if best == nil {
			best = a
			continue
		}
		c := compareSQLValues(a, best)
		if (name == "min" && c < 0) || (name == "max" && c > 0) {
			best = a
		}
	}
	return best, nil
}

// fnRound implements round(X[,Y]), which always returns a real and rounds
// halves away from zero.
func fnRound(args []any) (any, error) {
	if len(args) < 1 || len(args) > 2 {
		return nil, errors.New("round expects 1 or 2 arguments")
	}
	if args[0] == nil {
		return nil, nil
	}
	v, ok := asFloat(args[0])
	if !ok {
		return nil, errors.New("round expects a numeric argument")
	}
	digits := 0
	if len(args) == 2 {
		if args[1] == nil {
			return nil, nil
		}
		d, ok := asInt(args[1])
		if !ok {
			return nil, errors.New("round expects an integer digit count")
		}
		digits = int(d)
	}
	p := math.Pow(10, float64(digits))
	scaled := v * p
	// Round half away from zero.
	var r float64
	if scaled >= 0 {
		r = math.Floor(scaled + 0.5)
	} else {
		r = math.Ceil(scaled - 0.5)
	}
	return r / p, nil
}

// fnMod implements mod(X,Y), which always returns a real in SQLite.
func fnMod(args []any) (any, error) {
	if len(args) != 2 {
		return nil, errors.New("mod expects 2 arguments")
	}
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	x, ok := asFloat(args[0])
	if !ok {
		return nil, errors.New("mod expects numeric arguments")
	}
	y, ok := asFloat(args[1])
	if !ok {
		return nil, errors.New("mod expects numeric arguments")
	}
	if y == 0 {
		return nil, nil
	}
	return math.Mod(x, y), nil
}

// fnLike implements SQLite's LIKE with optional ESCAPE, ASCII case-insensitive.
func fnLike(args []any) (any, error) {
	if len(args) < 2 || len(args) > 3 {
		return nil, errors.New("like expects 2 or 3 arguments")
	}
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	pat := stringifySQL(args[0])
	s := stringifySQL(args[1])
	esc := rune(0)
	if len(args) == 3 {
		if args[2] == nil {
			return nil, nil
		}
		e := stringifySQL(args[2])
		if e != "" {
			r, _ := utf8.DecodeRuneInString(e)
			esc = r
		}
	}
	return matchLike(pat, s, esc), nil
}

func matchLike(pat, s string, esc rune) bool {
	p := []rune(pat)
	t := []rune(s)
	var pi, ti int
	starP, starT := -1, -1
	for ti < len(t) {
		if pi < len(p) {
			c := p[pi]
			if esc != 0 && c == esc && pi+1 < len(p) {
				pi++
				if toLowerRune(p[pi]) == toLowerRune(t[ti]) {
					pi++
					ti++
					continue
				}
			} else if c == '%' {
				starP = pi
				starT = ti
				pi++
				continue
			} else if c == '_' || toLowerRune(c) == toLowerRune(t[ti]) {
				pi++
				ti++
				continue
			}
		}
		if starP >= 0 {
			pi = starP + 1
			starT++
			ti = starT
			continue
		}
		return false
	}
	for pi < len(p) && p[pi] == '%' {
		pi++
	}
	return pi == len(p)
}

func toLowerRune(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + 32
	}
	return r
}

// fnGlob implements SQLite's GLOB: case-sensitive with *, ? and [...] classes.
func fnGlob(args []any) (any, error) {
	if len(args) != 2 {
		return nil, errors.New("glob expects 2 arguments")
	}
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	return matchGlob(stringifySQL(args[0]), stringifySQL(args[1])), nil
}

func matchGlob(pat, s string) bool {
	p := []rune(pat)
	t := []rune(s)
	return globMatch(p, t)
}

func globMatch(p, t []rune) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			for i := 0; i <= len(t); i++ {
				if globMatch(p[1:], t[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(t) == 0 {
				return false
			}
			p, t = p[1:], t[1:]
		case '[':
			if len(t) == 0 {
				return false
			}
			p = p[1:]
			neg := false
			if len(p) > 0 && (p[0] == '^' || p[0] == '!') {
				neg = true
				p = p[1:]
			}
			matched := false
			for len(p) > 0 && p[0] != ']' {
				if len(p) >= 2 && p[1] == '-' && p[0] != ']' {
					if t[0] >= p[0] && t[0] <= p[2] {
						matched = true
					}
					p = p[3:]
					continue
				}
				if p[0] == t[0] {
					matched = true
				}
				p = p[1:]
			}
			if len(p) > 0 {
				p = p[1:] // consume ']'
			}
			if matched == neg {
				return false
			}
			t = t[1:]
		default:
			if len(t) == 0 || p[0] != t[0] {
				return false
			}
			p, t = p[1:], t[1:]
		}
	}
	return len(t) == 0
}
