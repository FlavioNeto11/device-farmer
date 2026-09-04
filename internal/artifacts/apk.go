package artifacts

// APK metadata extraction, with no aapt.
//
// farm.artifacts.package and version_code are what let a job say "install the
// build of com.acme.app with version code >= 4711" instead of naming a path,
// and what let the runner decide whether an install is even needed. To fill
// them we have to read AndroidManifest.xml out of the APK.
//
// The obvious tool is `aapt dump badging`. The farm will not have it: farmd
// runs in a container on a Linux host next to a USB hub, not on a machine with
// an Android SDK, and adding a 100 MB SDK dependency plus an exec boundary to
// read four attributes is a bad trade. Shelling out would also mean a parse
// failure arrives as an exit status and a line of English on stderr, which is
// no easier to handle than the bytes themselves.
//
// So we read the format. An APK is a zip; AndroidManifest.xml inside it is not
// text but AXML — Android's binary XML, a chunked format with a string pool at
// the front. What follows is the smallest reader that can find four values in
// it: package, versionCode, versionCodeMajor and versionName. It is
// deliberately not a general AXML decoder; it does not resolve resource
// references, styles or namespaces beyond recognising the android one, and it
// stops at the first <manifest> element.
//
// Every read is bounds-checked and every loop is guaranteed to advance,
// because an APK is untrusted input: it arrives over an HTTP upload and is
// parsed inside the control plane. A panic here would take down the process
// that holds every lease in the farm.

import (
	"archive/zip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
)

// manifestEntry is the exact name Android requires. The lookup is
// case-sensitive on purpose: a zip entry called "androidmanifest.xml" is not
// the manifest, and treating it as one would let a crafted archive decide what
// package an artifact claims to be.
const manifestEntry = "AndroidManifest.xml"

// maxManifestBytes caps the decompressed manifest. Real manifests are a few
// tens of kilobytes; this bound is what stops a zip bomb from turning an
// upload into an out-of-memory kill of the control plane.
const maxManifestBytes = 8 << 20

// androidNS is the namespace URI of the android: attribute prefix.
const androidNS = "http://schemas.android.com/apk/res/android"

// AXML chunk types, from AOSP's ResourceTypes.h.
const (
	chunkStringPool      = 0x0001
	chunkXML             = 0x0003
	chunkXMLStartElement = 0x0102
	chunkXMLResourceMap  = 0x0180
)

// Res_value data types we care about. Anything else we treat as unreadable
// rather than guessing at a rendering.
const (
	typeString  = 0x03
	typeIntDec  = 0x10
	typeIntHex  = 0x11
	typeIntBool = 0x12
)

// Attribute resource ids from android.R.attr. These identify an attribute
// independently of the string pool, which is what keeps metadata readable on
// builds that have stripped attribute names, the android namespace URI, or
// both. A wrong constant here can only cost metadata on such a build: the
// name-and-namespace match beside it still recognises an ordinary APK.
const (
	resIDVersionCode      = 0x0101021b
	resIDVersionName      = 0x0101021c
	resIDVersionCodeMajor = 0x01010576
)

const (
	// noStringRef is the AXML encoding of "no string" (-1 as uint32).
	noStringRef = 0xFFFFFFFF

	chunkHeaderLen   = 8  // type, headerSize, size
	xmlNodeHeaderLen = 16 // chunk header plus lineNumber and comment
	attrExtLen       = 20 // ns, name, attributeStart/Size/Count, id/class/styleIndex
	minAttrLen       = 20 // ns, name, rawValue, Res_value
)

// ErrNoManifest means the zip had no AndroidManifest.xml entry, which is what
// happens when a .aab, an .xapk or a plain zip is uploaded as kind 'apk'.
var ErrNoManifest = errors.New("artifacts: archive contains no AndroidManifest.xml; upload an .apk, not an .aab, .xapk or plain zip")

// Manifest is the slice of AndroidManifest.xml the farm records.
type Manifest struct {
	// Package is the application id, e.g. "com.acme.app".
	Package string

	// VersionCode is the LONG version code: versionCodeMajor shifted left by
	// 32 and or-ed with versionCode, matching PackageInfo.getLongVersionCode.
	// For the overwhelming majority of APKs, which have no major component,
	// this is just versionCode. Ordering by it therefore orders builds
	// correctly in both cases, which is the point of storing it.
	VersionCode int64

	// VersionName is the human-facing string, e.g. "3.1.4-rc2". Parsed for
	// logging and display; farm.artifacts has no column for it.
	VersionName string
}

// ParseAPK reads the manifest out of an APK.
//
// r must support random access because the zip central directory lives at the
// end of the archive. size is the total length of the archive.
func ParseAPK(r io.ReaderAt, size int64) (Manifest, error) {
	if size <= 0 {
		return Manifest{}, errors.New("artifacts: empty archive")
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return Manifest{}, fmt.Errorf("artifacts: not a readable zip: %w", err)
	}
	for _, f := range zr.File {
		if f.Name != manifestEntry {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return Manifest{}, fmt.Errorf("artifacts: open %s: %w", manifestEntry, err)
		}
		// The limit is applied to the DECOMPRESSED stream rather than trusting
		// f.UncompressedSize64, which comes from the archive's own headers and
		// is exactly the field a hostile zip lies about. One byte past the cap
		// is read so an oversized manifest is detected rather than silently
		// truncated into a parse error.
		raw, err := io.ReadAll(io.LimitReader(rc, maxManifestBytes+1))
		rc.Close()
		if err != nil {
			return Manifest{}, fmt.Errorf("artifacts: read %s: %w", manifestEntry, err)
		}
		if len(raw) > maxManifestBytes {
			return Manifest{}, fmt.Errorf("artifacts: %s exceeds %d bytes", manifestEntry, maxManifestBytes)
		}
		return ParseAXML(raw)
	}
	return Manifest{}, ErrNoManifest
}

// ParseAXML extracts the manifest fields from a binary AndroidManifest.xml.
//
// Exported separately from ParseAPK so the decoder can be exercised against a
// manifest on its own, without building a zip around it.
func ParseAXML(b []byte) (Manifest, error) {
	typ, headerSize, chunkSize, ok := chunkHeader(b, 0)
	if !ok {
		return Manifest{}, errors.New("artifacts: manifest is too short, or claims an implausible size")
	}
	if typ != chunkXML {
		return Manifest{}, fmt.Errorf("artifacts: manifest is not binary XML (chunk type 0x%04x)", typ)
	}
	// Stop at whichever ends first, the buffer or the size the header claims.
	// A truncated download over-claims, and reading the chunks that did arrive
	// beats reading none: the string pool and <manifest> sit near the front,
	// so a partial file is often still worth its package name.
	end := chunkSize
	if end > len(b) {
		end = len(b)
	}

	var pool *stringPool
	var resMap []uint32

	for off := headerSize; off+chunkHeaderLen <= end; {
		ctype, _, csize, ok := chunkHeader(b, off)
		if !ok {
			return Manifest{}, errors.New("artifacts: truncated chunk in manifest")
		}
		// A chunk that does not advance the cursor would spin forever, and a
		// chunk that runs past the document is corrupt. Both are refused here
		// so no reader below has to defend itself against them. The sum is
		// widened first: csize comes off the wire and must not be able to wrap
		// into a small positive offset.
		if csize < chunkHeaderLen || int64(off)+int64(csize) > int64(end) {
			return Manifest{}, fmt.Errorf("artifacts: malformed chunk size %d at offset %d", csize, off)
		}
		body := b[off : off+csize]

		switch ctype {
		case chunkStringPool:
			p, err := parseStringPool(body)
			if err != nil {
				return Manifest{}, err
			}
			pool = p
		case chunkXMLResourceMap:
			resMap = parseResourceMap(body)
		case chunkXMLStartElement:
			if pool == nil {
				return Manifest{}, errors.New("artifacts: manifest element precedes its string pool")
			}
			name, attrs, err := parseStartElement(body, pool)
			if err != nil {
				return Manifest{}, err
			}
			// Anything nested under <manifest> is out of scope, so a
			// non-matching element just means "keep walking".
			if name == "manifest" {
				return manifestFrom(attrs, pool, resMap), nil
			}
		}
		off += csize
	}
	return Manifest{}, errors.New("artifacts: no <manifest> element found")
}

// manifestFrom picks the four attributes we store out of <manifest>'s list.
func manifestFrom(attrs []attribute, pool *stringPool, resMap []uint32) Manifest {
	var m Manifest
	var code, major int64
	var haveCode, haveMajor bool

	for _, a := range attrs {
		ns := pool.at(a.ns)
		name := pool.at(a.name)
		// aapt2 keeps both the attribute name and the android namespace URI in
		// the pool; repackagers strip one, the other, or both, leaving only the
		// resource map. Matching on the id AS WELL AS the name covers every
		// combination, and an id is proof of identity on its own: it is the
		// compiled android.R.attr constant, not a guess. Requiring name == ""
		// before consulting it — as this did — lost the version code of any
		// build that kept its names but dropped the namespace.
		id := uint32(0)
		if uint64(a.name) < uint64(len(resMap)) {
			id = resMap[a.name]
		}

		switch {
		case ns == "" && name == "package":
			m.Package = sanitizeText(a.stringValue(pool))
		case id == resIDVersionCode || (ns == androidNS && name == "versionCode"):
			if v, ok := a.intValue(pool); ok {
				code, haveCode = v, true
			}
		case id == resIDVersionCodeMajor || (ns == androidNS && name == "versionCodeMajor"):
			if v, ok := a.intValue(pool); ok {
				major, haveMajor = v, true
			}
		case id == resIDVersionName || (ns == androidNS && name == "versionName"):
			m.VersionName = sanitizeText(a.stringValue(pool))
		}
	}

	// Both halves came off untrusted bytes, and intValue's string branch can
	// return anything ParseInt accepts. Android types versionCode and
	// versionCodeMajor as signed 32-bit ints, so a value outside that range is
	// a forged manifest — and a wide major shifted left by 32 wraps into a
	// NEGATIVE version_code, which then sorts ahead of every genuine build in
	// the artifacts_pkg index and quietly wins "latest build" queries.
	if code < 0 || code > math.MaxUint32 {
		code, haveCode = 0, false
	}
	if major < 0 || major > math.MaxInt32 {
		major, haveMajor = 0, false
	}
	if haveCode || haveMajor {
		m.VersionCode = major<<32 | code
	}
	return m
}

// ---------------------------------------------------------------------
// Chunk readers
// ---------------------------------------------------------------------

// chunkHeader reads the 8-byte header every AXML chunk starts with. It only
// reads; deciding whether a size is plausible for its position is the caller's
// job, since the outer chunk is measured against the buffer and inner ones
// against the document end.
func chunkHeader(b []byte, off int) (ctype, headerSize, size int, ok bool) {
	t, ok1 := u16at(b, off)
	h, ok2 := u16at(b, off+2)
	s, ok3 := u32at(b, off+4)
	// The 2^31 ceiling is what keeps every later `off + size` inside int
	// range, so no bounds check downstream can be defeated by a wrap.
	if !ok1 || !ok2 || !ok3 || uint64(s) > 1<<31 {
		return 0, 0, 0, false
	}
	hs := int(h)
	if hs < chunkHeaderLen {
		// A header shorter than the header itself would place the body inside
		// it. Clamp so the walker still advances past the chunk.
		hs = chunkHeaderLen
	}
	return int(t), hs, int(s), true
}

// stringPool is the AXML string table. Strings are decoded eagerly: the pool
// of a manifest is small, and decoding once up front means every lookup below
// is a bounds-checked slice index instead of another parse of untrusted bytes.
type stringPool struct {
	strs []string
}

// at resolves a string reference, returning "" for the sentinel and for any
// index the pool does not contain.
func (p *stringPool) at(ref uint32) string {
	if p == nil || ref == noStringRef || uint64(ref) >= uint64(len(p.strs)) {
		return ""
	}
	return p.strs[ref]
}

const (
	poolHeaderLen = 28
	poolUTF8Flag  = 1 << 8
	// maxPoolStrings bounds the offset table so a forged count cannot make us
	// allocate gigabytes before the first bounds check fails.
	maxPoolStrings = 1 << 20
)

func parseStringPool(chunk []byte) (*stringPool, error) {
	if len(chunk) < poolHeaderLen {
		return nil, errors.New("artifacts: truncated string pool")
	}
	_, headerSize, _, ok := chunkHeader(chunk, 0)
	if !ok {
		return nil, errors.New("artifacts: unreadable string pool header")
	}
	count, ok1 := u32at(chunk, 8)
	styleCount, ok2 := u32at(chunk, 12)
	flags, ok3 := u32at(chunk, 16)
	stringsStart, ok4 := u32at(chunk, 20)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return nil, errors.New("artifacts: truncated string pool header")
	}
	if count > maxPoolStrings || styleCount > maxPoolStrings {
		return nil, fmt.Errorf("artifacts: implausible string pool (%d strings, %d styles)", count, styleCount)
	}
	// The offset table is four bytes per string and starts right after the
	// header. Checking that it FITS before allocating is what stops a forged
	// count from reserving a million string headers out of a chunk that could
	// not possibly describe them; the per-entry read below would catch it, but
	// only after the allocation it was meant to prevent.
	if uint64(headerSize)+4*uint64(count) > uint64(len(chunk)) {
		return nil, errors.New("artifacts: string pool offset table does not fit its chunk")
	}
	if uint64(stringsStart) > uint64(len(chunk)) {
		return nil, errors.New("artifacts: string pool data starts past the end of its chunk")
	}
	utf8 := flags&poolUTF8Flag != 0
	data := chunk[stringsStart:]

	strs := make([]string, count)
	for i := 0; i < int(count); i++ {
		off, ok := u32at(chunk, headerSize+4*i)
		if !ok {
			return nil, errors.New("artifacts: truncated string pool offset table")
		}
		if uint64(off) >= uint64(len(data)) {
			// A single bad offset is not worth discarding the whole manifest;
			// the entry stays empty and attribute matching falls through to
			// the resource-map path.
			continue
		}
		if utf8 {
			strs[i] = decodeUTF8String(data, int(off))
		} else {
			strs[i] = decodeUTF16String(data, int(off))
		}
	}
	return &stringPool{strs: strs}, nil
}

// decodeUTF16String reads a length-prefixed UTF-16LE string. The length is one
// uint16, or two when the high bit of the first is set.
func decodeUTF16String(b []byte, off int) string {
	n, ok := u16at(b, off)
	if !ok {
		return ""
	}
	off += 2
	length := int(n)
	if n&0x8000 != 0 {
		lo, ok := u16at(b, off)
		if !ok {
			return ""
		}
		off += 2
		length = int(uint32(n&0x7FFF)<<16 | uint32(lo))
	}
	// Widened before the comparison: a two-word length can reach 2^31-1 code
	// units, and 2*length would wrap on a 32-bit build.
	if length < 0 || int64(off)+2*int64(length) > int64(len(b)) {
		return ""
	}
	units := make([]uint16, length)
	for i := 0; i < length; i++ {
		units[i] = binary.LittleEndian.Uint16(b[off+2*i:])
	}
	// utf16.Decode substitutes U+FFFD for unpaired surrogates, so the result
	// is always valid UTF-8 even when the pool is not.
	return string(utf16.Decode(units))
}

// decodeUTF8String reads AXML's UTF-8 form: a character count, then a byte
// count, each one or two bytes, then the bytes.
func decodeUTF8String(b []byte, off int) string {
	_, off, ok := varLen8(b, off) // character count, unused: we want bytes
	if !ok {
		return ""
	}
	n, off, ok := varLen8(b, off)
	if !ok || off+n > len(b) {
		return ""
	}
	// Not validated here: sanitizeText replaces invalid sequences before any
	// of this reaches Postgres.
	return string(b[off : off+n])
}

// varLen8 reads AXML's one-or-two-byte length prefix.
func varLen8(b []byte, off int) (val, next int, ok bool) {
	if off < 0 || off >= len(b) {
		return 0, off, false
	}
	c := b[off]
	off++
	if c&0x80 == 0 {
		return int(c), off, true
	}
	if off >= len(b) {
		return 0, off, false
	}
	lo := b[off]
	off++
	return int(uint32(c&0x7F)<<8 | uint32(lo)), off, true
}

// parseResourceMap reads the string-pool-index to resource-id table. A short
// or absent map is not an error: it only matters for manifests whose attribute
// names were stripped.
func parseResourceMap(chunk []byte) []uint32 {
	_, headerSize, _, ok := chunkHeader(chunk, 0)
	if !ok || headerSize > len(chunk) {
		return nil
	}
	n := (len(chunk) - headerSize) / 4
	if n <= 0 || n > maxPoolStrings {
		return nil
	}
	ids := make([]uint32, n)
	for i := 0; i < n; i++ {
		v, ok := u32at(chunk, headerSize+4*i)
		if !ok {
			return ids[:i]
		}
		ids[i] = v
	}
	return ids
}

// attribute is one entry of a start element's attribute list.
type attribute struct {
	ns       uint32 // string ref
	name     uint32 // string ref
	rawValue uint32 // string ref, noStringRef when the compiler dropped it
	dataType uint8
	data     uint32
}

// stringValue renders the attribute as text, preferring the raw source string
// the compiler kept and falling back to the typed value.
func (a attribute) stringValue(pool *stringPool) string {
	if a.rawValue != noStringRef {
		if s := pool.at(a.rawValue); s != "" {
			return s
		}
	}
	switch a.dataType {
	case typeString:
		return pool.at(a.data)
	case typeIntDec, typeIntHex, typeIntBool:
		return strconv.FormatUint(uint64(a.data), 10)
	default:
		return ""
	}
}

// intValue renders the attribute as an integer.
//
// data is read as UNSIGNED. An Android version code is documented as a
// positive integer, so the unsigned reading is the one that never turns a
// large legitimate value into a negative row in farm.artifacts.
func (a attribute) intValue(pool *stringPool) (int64, bool) {
	switch a.dataType {
	case typeIntDec, typeIntHex, typeIntBool:
		return int64(a.data), true
	case typeString:
		// Some build tools leave the version code as a literal string.
		s := strings.TrimSpace(pool.at(a.data))
		if s == "" && a.rawValue != noStringRef {
			s = strings.TrimSpace(pool.at(a.rawValue))
		}
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	default:
		return 0, false
	}
}

// parseStartElement reads an element's name and attribute list.
func parseStartElement(chunk []byte, pool *stringPool) (string, []attribute, error) {
	nameRef, ok := u32at(chunk, xmlNodeHeaderLen+4)
	if !ok {
		return "", nil, errors.New("artifacts: truncated start element")
	}
	attrStart, ok1 := u16at(chunk, xmlNodeHeaderLen+8)
	attrSize, ok2 := u16at(chunk, xmlNodeHeaderLen+10)
	attrCount, ok3 := u16at(chunk, xmlNodeHeaderLen+12)
	if !ok1 || !ok2 || !ok3 {
		return "", nil, errors.New("artifacts: truncated start element header")
	}
	name := pool.at(nameRef)
	if attrCount == 0 {
		return name, nil, nil
	}
	// attributeSize is declared per element so future AOSP releases can grow
	// the record; anything smaller than the fields we read is corrupt.
	stride := int(attrSize)
	if stride < minAttrLen {
		return "", nil, fmt.Errorf("artifacts: attribute stride %d is below the format minimum", stride)
	}
	base := xmlNodeHeaderLen + int(attrStart)
	if base < xmlNodeHeaderLen+attrExtLen || base > len(chunk) {
		return "", nil, errors.New("artifacts: attribute table starts outside its element")
	}

	attrs := make([]attribute, 0, attrCount)
	for i := 0; i < int(attrCount); i++ {
		// Widened: count and stride are both 16-bit wire values whose product
		// exceeds an int32.
		at := int64(base) + int64(i)*int64(stride)
		if at+minAttrLen > int64(len(chunk)) {
			// A truncated tail costs us the attributes we did not reach, not
			// the ones we already read: a manifest that lost its last
			// attribute still tells us its package.
			break
		}
		off := int(at)
		ns, _ := u32at(chunk, off)
		nm, _ := u32at(chunk, off+4)
		raw, _ := u32at(chunk, off+8)
		// Res_value: uint16 size, uint8 res0, uint8 dataType, uint32 data.
		dt := chunk[off+15]
		data, _ := u32at(chunk, off+16)
		attrs = append(attrs, attribute{ns: ns, name: nm, rawValue: raw, dataType: dt, data: data})
	}
	return name, attrs, nil
}

// ---------------------------------------------------------------------
// Bounds-checked primitives. Every multi-byte read in this file goes through
// one of these, so a malformed APK yields an empty field rather than a panic.
// ---------------------------------------------------------------------

func u16at(b []byte, off int) (uint16, bool) {
	if off < 0 || off+2 > len(b) {
		return 0, false
	}
	return binary.LittleEndian.Uint16(b[off:]), true
}

func u32at(b []byte, off int) (uint32, bool) {
	if off < 0 || off+4 > len(b) {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b[off:]), true
}
