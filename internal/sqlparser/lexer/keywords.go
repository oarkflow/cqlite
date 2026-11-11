package lexer

// keywords maps lowercase SQL keywords to their token types.
// Uses a hash-map approach with FNV-1a for O(1) lookup with zero allocations.

// kwEntry is a keyword table entry in the hash map.
type kwEntry struct {
	word string
	tok  TokenType
}

// Hash map for keyword lookup. Size is a power of 2.
const kwMapSize = 512 // must be power of 2
const kwMapMask = kwMapSize - 1

// Each slot holds a small bucket (typically 0-1 entries after hashing).
var kwMap [kwMapSize][]kwEntry

// fnv1a computes FNV-1a hash of bytes.
func fnv1a(b []byte) uint32 {
	h := uint32(2166136261)
	for _, c := range b {
		h ^= uint32(c)
		h *= 16777619
	}
	return h
}

// fnv1aStr computes FNV-1a hash of a string without allocation.
func fnv1aStr(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func init() {
	words := []kwEntry{
		{"add", ADD},
		{"after", AFTER},
		{"all", ALL},
		{"alter", ALTER},
		{"analyze", ANALYZE},
		{"and", AND},
		{"as", AS},
		{"asc", ASC},
		{"auto_increment", AUTO_INCREMENT},
		{"between", BETWEEN},
		{"bigint", BIGINT},
		{"binary", BINARY},
		{"blob", BLOB},
		{"boolean", BOOLEAN},
		{"by", BY},
		{"cascade", CASCADE},
		{"case", CASE},
		{"cast", CAST},
		{"change", CHANGE},
		{"char", CHAR},
		{"character", CHARACTER},
		{"check", CHECK},
		{"collate", COLLATE},
		{"column", COLUMN},
		{"comment", COMMENT_KW},
		{"constraint", CONSTRAINT},
		{"create", CREATE},
		{"cross", CROSS},
		{"database", DATABASE},
		{"date", DATE},
		{"datetime", DATETIME},
		{"decimal", DECIMAL},
		{"default", DEFAULT},
		{"deferrable", DEFERRABLE},
		{"deferred", DEFERRED},
		{"delete", DELETE},
		{"desc", DESC},
		{"distinct", DISTINCT},
		{"double", DOUBLE},
		{"drop", DROP},
		{"else", ELSE},
		{"end", END},
		{"engine", ENGINE},
		{"enum", ENUM},
		{"escape", ESCAPE},
		{"except", EXCEPT},
		{"exists", EXISTS},
		{"explain", EXPLAIN},
		{"false", FALSE_KW},
		{"filter", FILTER},
		{"first", FIRST},
		{"float", FLOAT_KW},
		{"for", FOR},
		{"foreign", FOREIGN},
		{"from", FROM},
		{"full", FULL},
		{"function", FUNCTION},
		{"group", GROUP},
		{"having", HAVING},
		{"if", IF},
		{"ignore", IGNORE},
		{"in", IN},
		{"index", INDEX},
		{"inner", INNER},
		{"insert", INSERT},
		{"int", INT_KW},
		{"integer", INTEGER},
		{"intersect", INTERSECT},
		{"into", INTO},
		{"is", IS},
		{"join", JOIN},
		{"json", JSON},
		{"jsonb", JSONB},
		{"key", KEY},
		{"last", LAST},
		{"lateral", LATERAL},
		{"left", LEFT},
		{"like", LIKE},
		{"limit", LIMIT},
		{"longblob", LONGBLOB},
		{"longtext", LONGTEXT},
		{"match", MATCH},
		{"materialized", MATERIALIZED},
		{"mediumblob", MEDIUMBLOB},
		{"mediumint", MEDIUMINT},
		{"mediumtext", MEDIUMTEXT},
		{"natural", NATURAL},
		{"nchar", NCHAR},
		{"no", NO},
		{"not", NOT},
		{"null", NULL_KW},
		{"nulls", NULLS},
		{"numeric", NUMERIC},
		{"of", OF},
		{"offset", OFFSET},
		{"on", ON},
		{"or", OR},
		{"order", ORDER},
		{"outer", OUTER},
		{"over", OVER},
		{"partition", PARTITION},
		{"primary", PRIMARY},
		{"procedure", PROCEDURE},
		{"real", REAL},
		{"recursive", RECURSIVE},
		{"references", REFERENCES},
		{"rename", RENAME},
		{"replace", REPLACE},
		{"restrict", RESTRICT},
		{"returning", RETURNING},
		{"right", RIGHT},
		{"rollback", ROLLBACK},
		{"select", SELECT},
		{"set", SET},
		{"show", SHOW},
		{"smallint", SMALLINT},
		{"table", TABLE},
		{"tables", TABLES},
		{"text", TEXT},
		{"then", THEN},
		{"time", TIME},
		{"timestamp", TIMESTAMP},
		{"tinyblob", TINYBLOB},
		{"tinyint", TINYINT},
		{"tinytext", TINYTEXT},
		{"to", TO},
		{"transaction", TRANSACTION},
		{"trigger", TRIGGER},
		{"true", TRUE_KW},
		{"truncate", TRUNCATE},
		{"union", UNION},
		{"unique", UNIQUE},
		{"update", UPDATE},
		{"use", USE},
		{"using", USING},
		{"values", VALUES},
		{"varbinary", VARBINARY},
		{"varchar", VARCHAR},
		{"view", VIEW},
		{"when", WHEN},
		{"where", WHERE},
		{"window", WINDOW},
		{"with", WITH},
		{"without", WITHOUT},
		{"year", YEAR},
	}
	for _, e := range words {
		h := fnv1aStr(e.word) & kwMapMask
		kwMap[h] = append(kwMap[h], e)
	}
}

// lookupKeyword returns the token for a keyword, or IDENT if not found.
// val must be lowercase. This function performs zero allocations.
func lookupKeyword(val []byte) TokenType {
	l := len(val)
	if l == 0 || l > 14 {
		return IDENT
	}
	h := fnv1a(val) & kwMapMask
	bucket := kwMap[h]
	for i := range bucket {
		if len(bucket[i].word) == l && bytesEqualString(val, bucket[i].word) {
			return bucket[i].tok
		}
	}
	return IDENT
}

func bytesEqualString(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	// Compare first and last bytes for early rejection
	n := len(b)
	if n > 0 && (b[0] != s[0] || b[n-1] != s[n-1]) {
		return false
	}
	for i := 1; i < n-1; i++ {
		if b[i] != s[i] {
			return false
		}
	}
	return true
}
