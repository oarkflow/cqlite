package db

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// callTimeFunc implements SQLite's date/time scalar functions. A time value is
// modelled as a Julian day number so that arithmetic and modifiers behave the
// way SQLite's own implementation does.
func callTimeFunc(name string, args []any) (any, bool, error) {
	switch name {
	case "date", "time", "datetime", "julianday", "unixepoch":
		if len(args) < 1 {
			return nil, true, fmt.Errorf("%s expects at least 1 argument", name)
		}
		jd, hasTime, err := resolveTimeValue(args[0])
		if err != nil {
			return nil, true, err
		}
		if jd == 0 {
			return nil, true, nil
		}
		if len(args) > 1 {
			jd, err = applyTimeModifiers(jd, hasTime, args[1:])
			if err != nil {
				return nil, true, err
			}
		}
		switch name {
		case "date":
			return formatDate(jd), true, nil
		case "time":
			return formatTime(jd), true, nil
		case "datetime":
			return formatDateTime(jd), true, nil
		case "julianday":
			return jd, true, nil
		case "unixepoch":
			return int64(math.Floor(jdUnix(jd))), true, nil
		}
	case "strftime":
		if len(args) < 2 {
			return nil, true, errors.New("strftime expects at least 2 arguments")
		}
		format, _ := argText(args, 0, "strftime")
		jd, hasTime, err := resolveTimeValue(args[1])
		if err != nil {
			return nil, true, err
		}
		if jd == 0 {
			return nil, true, nil
		}
		if len(args) > 2 {
			jd, err = applyTimeModifiers(jd, hasTime, args[2:])
			if err != nil {
				return nil, true, err
			}
		}
		return formatStrftime(format, jd), true, nil
	case "timediff":
		if len(args) != 2 {
			return nil, true, errors.New("timediff expects 2 arguments")
		}
		a, _, err := resolveTimeValue(args[0])
		if err != nil {
			return nil, true, err
		}
		b, _, err := resolveTimeValue(args[1])
		if err != nil {
			return nil, true, err
		}
		if a == 0 || b == 0 {
			return nil, true, nil
		}
		return formatTimeInterval(a - b), true, nil
	}
	return nil, false, nil
}

// resolveTimeValue converts a SQL time value into a Julian day number. It
// returns 0 when the value is NULL or unparsable, which callers surface as SQL
// NULL just like SQLite does.
func resolveTimeValue(v any) (float64, bool, error) {
	if v == nil {
		return 0, false, nil
	}
	if isIntegerValue(v) {
		// A bare integer is a Julian day number.
		i, _ := asInt(v)
		return float64(i), true, nil
	}
	if f, ok := asFloat(v); ok {
		return f, true, nil
	}
	s := strings.TrimSpace(stringifySQL(v))
	if s == "" {
		return 0, false, nil
	}
	if strings.EqualFold(s, "now") {
		return timeToJD(time.Now().UTC()), true, nil
	}
	// 8-digit strings are Julian days; 10+ digit strings are unix timestamps.
	if isAllDigits(s) {
		if len(s) == 8 {
			f, err := strconv.ParseFloat(s, 64)
			return f, true, err
		}
		if len(s) >= 10 {
			secs, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return 0, false, nil
			}
			return unixToJD(float64(secs)), true, nil
		}
	}
	return parseTimeText(s)
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseTimeText parses SQLite's textual date/time formats.
func parseTimeText(s string) (float64, bool, error) {
	// Split the date and time parts at ' ' or 'T'.
	var datePart, timePart string
	if i := strings.IndexAny(s, " tT"); i >= 0 {
		datePart, timePart = s[:i], strings.TrimSpace(s[i+1:])
		// A leading 'T' with no date is not a separator.
		if datePart == "" {
			timePart = s
		}
	} else {
		if strings.Contains(s, ":") {
			timePart = s
		} else {
			datePart = s
		}
	}

	y, mo, d := 2000, 1, 1
	if datePart != "" {
		parts := strings.Split(datePart, "-")
		if len(parts) > 3 {
			return 0, false, nil
		}
		var err error
		if y, err = strconv.Atoi(parts[0]); err != nil {
			return 0, false, nil
		}
		if len(parts) >= 2 {
			if mo, err = strconv.Atoi(parts[1]); err != nil {
				return 0, false, nil
			}
		}
		if len(parts) == 3 {
			if d, err = strconv.Atoi(parts[2]); err != nil {
				return 0, false, nil
			}
		}
		if mo < 1 || mo > 12 || d < 1 || d > 31 {
			return 0, false, nil
		}
	}

	h, mi, sec := 0, 0, 0
	frac := 0.0
	hasTime := timePart != ""
	if hasTime {
		// Strip fractional seconds.
		if i := strings.IndexByte(timePart, '.'); i >= 0 {
			f, err := strconv.ParseFloat("0"+timePart[i:], 64)
			if err != nil {
				return 0, false, nil
			}
			frac = f
			timePart = timePart[:i]
		}
		parts := strings.Split(timePart, ":")
		if len(parts) > 3 {
			return 0, false, nil
		}
		var err error
		if h, err = strconv.Atoi(parts[0]); err != nil {
			return 0, false, nil
		}
		if len(parts) >= 2 {
			if mi, err = strconv.Atoi(parts[1]); err != nil {
				return 0, false, nil
			}
		}
		if len(parts) == 3 {
			if sec, err = strconv.Atoi(parts[2]); err != nil {
				return 0, false, nil
			}
		}
		if h > 23 || mi > 59 || sec > 60 {
			return 0, false, nil
		}
	}
	jd := gregorianToJD(y, mo, d) + (float64(h)-12)/24 + float64(mi)/1440 + (float64(sec)+frac)/86400
	return jd, hasTime, nil
}

// gregorianToJD converts a proleptic Gregorian date to a Julian day number.
func gregorianToJD(y, m, d int) float64 {
	a := (14 - m) / 12
	yy := y + 4800 - a
	mm := m + 12*a - 3
	jdn := float64(d) + float64((153*mm+2)/5) + 365*float64(yy) + float64(yy/4) - float64(yy/100) + float64(yy/400) - 32045
	return jdn
}

// jdToGregorian converts a Julian day number back to a calendar date.
func jdToGregorian(jd float64) (y, m, d int) {
	z := math.Floor(jd + 0.5)
	a := z
	if z >= 2299161 {
		alpha := math.Floor((z - 1867216.25) / 36524.25)
		a = z + 1 + alpha - math.Floor(alpha/4)
	}
	b := a + 1524
	c := math.Floor((b - 122.1) / 365.25)
	d0 := math.Floor(365.25 * c)
	e := math.Floor((b - d0) / 30.6001)
	day := b - d0 - math.Floor(30.6001*e)
	month := e - 1
	if e >= 14 {
		month = e - 13
	}
	// January and February belong to the previous "calendar year" of the
	// Julian-day formula.
	year := c - 4715
	if month > 2 {
		year = c - 4716
	}
	return int(year), int(month), int(day)
}

// jdTimeOfDay splits the fractional day from a Julian day number.
func jdTimeOfDay(jd float64) (h, mi int, s float64) {
	frac := jd + 0.5
	frac = frac - math.Floor(frac)
	total := frac * 86400
	h = int(total) / 3600
	mi = (int(total) % 3600) / 60
	s = total - float64(h*3600+mi*60)
	// Guard against floating point drift pushing us past 23:59:59.999.
	if s >= 60 {
		s = 59.999999
	}
	return h, mi, s
}

func jdToWeekday(jd float64) int {
	// Julian day 0 was a Monday. Modulo gives Sunday = 0.
	w := int(math.Floor(jd+1.5)) % 7
	if w < 0 {
		w += 7
	}
	return w
}

func timeToJD(t time.Time) float64 {
	y, mo, d := t.Date()
	h, mi, s := t.Clock()
	ns := t.Nanosecond()
	return gregorianToJD(y, int(mo), d) + (float64(h)-12)/24 + float64(mi)/1440 + (float64(s)+float64(ns)/1e9)/86400
}

func jdUnix(jd float64) float64 {
	return (jd - 2440587.5) * 86400
}

func unixToJD(secs float64) float64 {
	return secs/86400 + 2440587.5
}

// applyTimeModifiers applies SQLite's date/time modifiers to a Julian day.
func applyTimeModifiers(jd float64, hasTime bool, mods []any) (float64, error) {
	_ = hasTime
	for _, m := range mods {
		if m == nil {
			continue
		}
		s := strings.TrimSpace(stringifySQL(m))
		if s == "" {
			continue
		}
		lower := strings.ToLower(s)
		// Phrase modifiers are matched whole before the "N units" form is
		// attempted, so "start of month" is not misread as an amount.
		switch lower {
		case "start of day", "start of month", "start of year", "start of week",
			"unixepoch", "localtime", "utc":
			next, err := applyPhraseModifier(jd, lower)
			if err != nil {
				return 0, err
			}
			jd = next
			continue
		}
		if strings.HasPrefix(lower, "weekday") {
			var target int
			if _, err := fmt.Sscanf(lower, "weekday %d", &target); err != nil {
				return 0, fmt.Errorf("unknown time modifier: %q", s)
			}
			w := jdToWeekday(jd)
			delta := (target - w + 7) % 7
			if delta == 0 {
				delta = 7
			}
			jd += float64(delta)
			continue
		}
		amount := 0.0
		unit := ""
		if i := strings.IndexAny(lower, " \t"); i >= 0 {
			unit = strings.TrimSpace(lower[i:])
			var err error
			amount, err = strconv.ParseFloat(strings.TrimSpace(lower[:i]), 64)
			if err != nil {
				return 0, fmt.Errorf("unknown time modifier: %q", s)
			}
		}
		switch unit {
		case "days", "day":
			jd += amount
		case "hours", "hour":
			jd += amount / 24
		case "minutes", "minute":
			jd += amount / 1440
		case "seconds", "second":
			jd += amount / 86400
		case "months", "month":
			jd = addMonths(jd, int(amount))
		case "years", "year":
			jd = addMonths(jd, int(amount)*12)
		default:
			return 0, fmt.Errorf("unknown time modifier: %q", s)
		}
	}
	return jd, nil
}

// applyPhraseModifier handles the word-form date/time modifiers.
func applyPhraseModifier(jd float64, lower string) (float64, error) {
	switch lower {
	case "start of day":
		y, mo, d := jdToGregorian(jd)
		return gregorianToJD(y, mo, d), nil
	case "start of month":
		y, mo, _ := jdToGregorian(jd)
		return gregorianToJD(y, mo, 1), nil
	case "start of year":
		y, _, _ := jdToGregorian(jd)
		return gregorianToJD(y, 1, 1), nil
	case "start of week":
		w := jdToWeekday(jd)
		// SQLite weeks start on Sunday.
		return math.Floor(jd+0.5) - float64(w) - 0.5, nil
	case "unixepoch":
		return unixToJD(jdUnix(jd)), nil
	case "localtime":
		return timeToJD(jdToTime(jd).Local()), nil
	case "utc":
		return timeToJD(jdToTime(jd).UTC()), nil
	}
	return jd, fmt.Errorf("unknown time modifier: %q", lower)
}

// addMonths shifts a Julian day by whole months, clamping the day of month to
// the length of the target month the way SQLite does.
func addMonths(jd float64, months int) float64 {
	y, m, d := jdToGregorian(jd)
	h, mi, s := jdTimeOfDay(jd)
	total := (y*12 + (m - 1)) + months
	ny := total / 12
	nm := total%12 + 1
	// SQLite does not clamp the day of month: 2024-01-31 plus one month is
	// 2024-03-02, not 2024-02-29. The date arithmetic overflows naturally.
	return gregorianToJD(ny, nm, d) + (float64(h)-12)/24 + float64(mi)/1440 + s/86400
}

func daysInMonth(y, m int) int {
	switch m {
	case 1, 3, 5, 7, 8, 10, 12:
		return 31
	case 4, 6, 9, 11:
		return 30
	case 2:
		if y%4 == 0 && (y%100 != 0 || y%400 == 0) {
			return 29
		}
		return 28
	}
	return 30
}

func jdToTime(jd float64) time.Time {
	y, m, d := jdToGregorian(jd)
	h, mi, s := jdTimeOfDay(jd)
	sec := int(s)
	nsec := int((s - float64(sec)) * 1e9)
	return time.Date(y, time.Month(m), d, h, mi, sec, nsec, time.UTC)
}

func formatDate(jd float64) string {
	y, m, d := jdToGregorian(jd)
	return fmt.Sprintf("%04d-%02d-%02d", y, m, d)
}

func formatTime(jd float64) string {
	h, mi, s := jdTimeOfDay(jd)
	sec := int(math.Floor(s))
	frac := s - float64(sec)
	if frac >= 0.5 {
		// SQLite rounds to millisecond precision for display.
	}
	ms := int(math.Round(frac * 1000))
	if ms >= 1000 {
		ms = 999
	}
	if ms == 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, mi, sec)
	}
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, mi, sec, ms)
}

func formatDateTime(jd float64) string {
	y, m, d := jdToGregorian(jd)
	h, mi, s := jdTimeOfDay(jd)
	sec := int(math.Floor(s))
	frac := s - float64(sec)
	ms := int(math.Round(frac * 1000))
	if ms >= 1000 {
		ms = 999
	}
	if ms == 0 {
		return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", y, m, d, h, mi, sec)
	}
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d.%03d", y, m, d, h, mi, sec, ms)
}

// formatTimeInterval renders a duration the way SQLite's timediff() does:
// a signed ISO-8601-ish interval with millisecond precision.
func formatTimeInterval(deltaJD float64) string {
	sign := "+"
	secs := deltaJD * 86400
	if secs < 0 {
		sign = "-"
		secs = -secs
	}
	totalMs := int64(math.Round(secs * 1000))
	days := totalMs / 86400000
	totalMs -= days * 86400000
	hours := totalMs / 3600000
	totalMs -= hours * 3600000
	mins := totalMs / 60000
	totalMs -= mins * 60000
	secsOnly := totalMs / 1000
	ms := totalMs % 1000
	return fmt.Sprintf("%s%04d-%02d-%02d %02d:%02d:%02d.%03d", sign, days, 0, 0, hours, mins, secsOnly, ms)
}

// formatStrftime implements SQLite's strftime() format specifiers.
func formatStrftime(format string, jd float64) string {
	y, mo, d := jdToGregorian(jd)
	h, mi, s := jdTimeOfDay(jd)
	sec := int(math.Floor(s))
	frac := s - float64(sec)
	ms := int(math.Round(frac * 1000))
	if ms >= 1000 {
		ms = 999
	}
	doy := dayOfYear(y, mo, d)
	wd := jdToWeekday(jd)

	var b strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 >= len(format) {
			b.WriteByte(format[i])
			continue
		}
		i++
		// padding flag
		pad := '0'
		if i < len(format) && (format[i] == '-' || format[i] == '_' || format[i] == '0') {
			pad = rune(format[i])
			i++
		}
		if i >= len(format) {
			b.WriteByte('%')
			break
		}
		verb := format[i]
		padNum := func(n int, width int) string {
			if pad == '-' {
				return strconv.Itoa(n)
			}
			s := strconv.Itoa(n)
			for len(s) < width {
				if pad == '_' {
					s = " " + s
				} else {
					s = "0" + s
				}
			}
			return s
		}
		switch verb {
		case 'd':
			b.WriteString(padNum(d, 2))
		case 'e':
			if pad == '0' {
				pad = ' '
			}
			b.WriteString(padNum(d, 2))
		case 'H':
			b.WriteString(padNum(h, 2))
		case 'I':
			hh := h % 12
			if hh == 0 {
				hh = 12
			}
			b.WriteString(padNum(hh, 2))
		case 'M':
			b.WriteString(padNum(mi, 2))
		case 'S':
			b.WriteString(padNum(sec, 2))
		case 'f':
			if ms == 0 {
				b.WriteString("00.000")
			} else {
				b.WriteString(fmt.Sprintf("%02d.%03d", sec, ms))
			}
		case 'j':
			b.WriteString(padNum(doy, 3))
		case 'J':
			b.WriteString(strconv.FormatFloat(jd, 'f', -1, 64))
		case 'm':
			b.WriteString(padNum(mo, 2))
		case 's':
			b.WriteString(strconv.FormatInt(int64(math.Floor(jdUnix(jd))), 10))
		case 'w':
			b.WriteString(strconv.Itoa(wd))
		case 'W':
			b.WriteString(padNum(weekOfYear(y, mo, d, wd), 2))
		case 'Y':
			if pad == '-' {
				b.WriteString(strconv.Itoa(y))
			} else {
				b.WriteString(padNum(y, 4))
			}
		case 'C':
			b.WriteString(padNum(y/100, 2))
		case 'y':
			b.WriteString(padNum(absInt(y%100), 2))
		case 'G':
			iy, _, _ := isoWeek(y, mo, d, wd)
			if pad == '-' {
				b.WriteString(strconv.Itoa(iy))
			} else {
				b.WriteString(padNum(iy, 4))
			}
		case 'g':
			iy, _, _ := isoWeek(y, mo, d, wd)
			b.WriteString(padNum(absInt(iy%100), 2))
		case 'V':
			_, iw, _ := isoWeek(y, mo, d, wd)
			b.WriteString(padNum(iw, 2))
		case 'p':
			if h < 12 {
				b.WriteString("AM")
			} else {
				b.WriteString("PM")
			}
		case 'R':
			b.WriteString(fmt.Sprintf("%02d:%02d", h, mi))
		case 'T':
			b.WriteString(fmt.Sprintf("%02d:%02d:%02d", h, mi, sec))
		case 'F':
			b.WriteString(fmt.Sprintf("%04d-%02d-%02d", y, mo, d))
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(verb)
		}
	}
	return b.String()
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func dayOfYear(y, m, d int) int {
	n := d
	for i := 1; i < m; i++ {
		n += daysInMonth(y, i)
	}
	return n
}

func weekOfYear(y, m, d, wd int) int {
	// Weeks begin on Sunday; the week containing Jan 1 is week 01.
	doy := dayOfYear(y, m, d)
	// weekday of Jan 1
	jan1WD := (wd - ((doy - 1) % 7) + 7) % 7
	offset := (doy + jan1WD - 1) / 7
	if jan1WD > 0 {
		// partial first week still counts as week 01
		if doy <= 7-jan1WD {
			return 1
		}
	}
	return offset + 1
}

// isoWeek returns the ISO 8601 week-based year, week number and weekday.
func isoWeek(y, m, d, wd int) (int, int, int) {
	isoWD := wd
	if isoWD == 0 {
		isoWD = 7
	}
	// Thursday of the current week determines the ISO year.
	thursdayJDN := gregorianToJD(y, m, d) + float64(4-isoWD)
	ty, tm, td := jdToGregorian(thursdayJDN)
	week := (dayOfYear(ty, tm, td)-1)/7 + 1
	return ty, week, isoWD
}
