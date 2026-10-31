package parser

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"

	"emperror.dev/errors"
	"github.com/Jeffail/gabs/v2"
	"github.com/apex/log"
	"github.com/beevik/etree"
	"github.com/buger/jsonparser"
	"github.com/icza/dyno"
	"github.com/magiconair/properties"
	"gopkg.in/yaml.v3"

	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/internal/ufs"
)

// The file parsing options that are available for a server configuration file.
const (
	File       = "file"
	Yaml       = "yaml"
	Properties = "properties"
	Ini        = "ini"
	Json       = "json"
	Xml        = "xml"
)

// maxTextScanTokenSize bounds how large a single line the "file" parser will buffer.
const maxTextScanTokenSize = 64 * 1024 * 1024

// maxConfigFileSize caps how large a configuration file we'll attempt to parse.
const maxConfigFileSize = 64 * 1024 * 1024

// maxXMLDepth is the deepest element nesting of an XML configuration file that
// is rewritten. Real configuration files are far shallower than this.
const maxXMLDepth = 512

// xmlDepth returns the element nesting depth of the tree rooted at root.
func xmlDepth(root *etree.Element) int {
	if root == nil {
		return 0
	}
	type frame struct {
		e *etree.Element
		d int
	}
	stack := []frame{{root, 1}}
	depth := 0
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.d > depth {
			depth = f.d
		}
		for _, c := range f.e.ChildElements() {
			stack = append(stack, frame{c, f.d + 1})
		}
	}
	return depth
}

type ReplaceValue struct {
	value     []byte
	valueType jsonparser.ValueType
}

// Value returns the underlying value of the replacement. Be aware that this
// can include escaped UTF-8 sequences that will need to be handled by the caller
// in order to avoid accidentally injecting invalid sequences into the running
// process.
//
// For example the expected value may be "§Foo" but you'll be working directly
// with "\u00a7FOo" for this value. This will cause user pain if not solved since
// that is clearly not the value they were expecting to be using.
func (cv *ReplaceValue) Value() []byte {
	return cv.value
}

// Type returns the underlying data type for the Value field.
func (cv *ReplaceValue) Type() jsonparser.ValueType {
	return cv.valueType
}

// String returns the value as a string representation. This will automatically
// handle casting the UTF-8 sequence into the expected value, switching something
// like "\u00a7Foo" into "§Foo".
func (cv *ReplaceValue) String() string {
	switch cv.Type() {
	case jsonparser.String:
		str, err := jsonparser.ParseString(cv.value)
		if err != nil {
			panic(errors.Wrap(err, "parser: could not parse value"))
		}
		return str
	case jsonparser.Null:
		return "<nil>"
	case jsonparser.Boolean:
		return string(cv.value)
	case jsonparser.Number:
		return string(cv.value)
	default:
		return "<invalid>"
	}
}

func (cv *ReplaceValue) Bytes() []byte {
	switch cv.Type() {
	case jsonparser.String:
		var stackbuf [64]byte
		bU, err := jsonparser.Unescape(cv.value, stackbuf[:])
		if err != nil {
			panic(errors.Wrap(err, "parser: could not parse value"))
		}
		return bU
	case jsonparser.Null:
		return []byte("<nil>")
	case jsonparser.Boolean:
		return cv.value
	case jsonparser.Number:
		return cv.value
	default:
		return []byte("<invalid>")
	}
}

type ConfigurationParser string

func (cp ConfigurationParser) String() string {
	return string(cp)
}

// ConfigurationFile defines a configuration file for the server startup. These
// will be looped over and modified before the server finishes booting.
type ConfigurationFile struct {
	FileName string                         `json:"file"`
	Parser   ConfigurationParser            `json:"parser"`
	Replace  []ConfigurationFileReplacement `json:"replace"`

	// Tracks Agent' configuration so that we can quickly get values
	// out of it when variables request it.
	configuration []byte
}

// UnmarshalJSON is a custom unmarshaler for configuration files. If there is an
// error while parsing out the replacements, don't fail the entire operation,
// just log a global warning so someone can find the issue, and return an empty
// array of replacements.
func (f *ConfigurationFile) UnmarshalJSON(data []byte) error {
	var m map[string]*json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	for _, key := range []string{"file", "parser", "replace"} {
		if m[key] == nil {
			return errors.Errorf("parser: configuration file is missing the %q field", key)
		}
	}

	if err := json.Unmarshal(*m["file"], &f.FileName); err != nil {
		return err
	}

	if err := json.Unmarshal(*m["parser"], &f.Parser); err != nil {
		return err
	}

	if err := json.Unmarshal(*m["replace"], &f.Replace); err != nil {
		log.WithField("file", f.FileName).WithField("error", err).Warn("failed to unmarshal configuration file replacement")

		f.Replace = []ConfigurationFileReplacement{}
	}

	return nil
}

// ConfigurationFileReplacement defines a single find/replace instance for a
// given server configuration file.
// Reads a key as text. The Panel encodes a numeric key such as "25565" as a JSON number,
// so numbers and booleans are accepted alongside strings.
func scalarString(data []byte, key string) (string, error) {
	value, dataType, _, err := jsonparser.Get(data, key)
	if err != nil {
		return "", err
	}

	switch dataType {
	case jsonparser.String:
		return jsonparser.ParseString(value)
	case jsonparser.Number, jsonparser.Boolean:
		return string(value), nil
	case jsonparser.Null:
		return "", nil
	default:
		return "", errors.New("parser: " + key + " must be a string, number, or boolean")
	}
}

type ConfigurationFileReplacement struct {
	Match       string       `json:"match"`
	IfValue     string       `json:"if_value"`
	ReplaceWith ReplaceValue `json:"replace_with"`
}

// UnmarshalJSON handles unmarshaling the JSON representation into a struct that
// provides more useful data to this functionality.
func (cfr *ConfigurationFileReplacement) UnmarshalJSON(data []byte) error {
	m, err := scalarString(data, "match")
	if err != nil {
		return err
	}

	cfr.Match = m

	iv, err := scalarString(data, "if_value")
	// We only check keypath here since match & replace_with should be present on all of
	// them, however if_value is optional.
	if err != nil && err != jsonparser.KeyPathNotFoundError {
		return err
	}
	cfr.IfValue = iv

	rw, dt, _, err := jsonparser.Get(data, "replace_with")
	if err != nil {
		if err != jsonparser.KeyPathNotFoundError {
			return err
		}

		// Okay, likely dealing with someone who forgot to upgrade their eggs, so in
		// that case, fallback to using the old key which was "value".
		rw, dt, _, err = jsonparser.Get(data, "value")
		if err != nil {
			return err
		}
	}

	cfr.ReplaceWith = ReplaceValue{
		value:     rw,
		valueType: dt,
	}

	return nil
}

type templatableConfig struct {
	Docker struct {
		Interface string `json:"interface"`
		Network   struct {
			Interface string `json:"interface"`
		} `json:"network"`
	} `json:"docker"`
}

func newTemplatableConfig(c *config.Configuration) templatableConfig {
	var t templatableConfig
	t.Docker.Interface = c.Docker.Network.Interface
	t.Docker.Network.Interface = c.Docker.Network.Interface
	return t
}

// Parse parses a given configuration file and updates all the values within
// as defined in the API response from the Panel.
func (f *ConfigurationFile) Parse(file ufs.File) error {
	// log.WithField("path", path).WithField("parser", f.Parser.String()).Debug("parsing server configuration file")

	// Refuse to parse files larger than the cap. Every parser below buffers the
	// whole file in memory, and the contents are untrusted server-owned input, so
	// this guards the daemon against being OOM'd by an oversized config. The
	// server is still free to boot with the file as-is; we just don't rewrite it.
	if info, err := file.Stat(); err != nil {
		return err
	} else if info.Size() > maxConfigFileSize {
		return errors.Errorf("parser: refusing to parse configuration file %q: size %d exceeds limit of %d bytes", file.Name(), info.Size(), maxConfigFileSize)
	}

	if mb, err := json.Marshal(newTemplatableConfig(config.Get())); err != nil {
		return err
	} else {
		f.configuration = mb
	}

	var err error

	switch f.Parser {
	case Properties:
		err = f.parsePropertiesFile(file)
	case File:
		err = f.parseTextFile(file)
	case Yaml, "yml":
		err = f.parseYamlFile(file)
	case Json:
		err = f.parseJsonFile(file)
	case Ini:
		err = f.parseIniFile(file)
	case Xml:
		err = f.parseXmlFile(file)
	}
	return err
}

// Parses an xml file.
func (f *ConfigurationFile) parseXmlFile(file ufs.File) error {
	doc := etree.NewDocument()
	if _, err := doc.ReadFrom(io.LimitReader(file, maxConfigFileSize)); err != nil {
		return err
	}
	// The server still boots with the file as it is, the same as for files over
	// the size limit.
	if depth := xmlDepth(doc.Root()); depth > maxXMLDepth {
		return errors.Errorf("parser: refusing to parse configuration file %q: element nesting depth %d exceeds limit of %d", file.Name(), depth, maxXMLDepth)
	}

	// If there is no root we should create a basic start to the file. This isn't required though,
	// and if it doesn't work correctly I'll just remove the code.
	if doc.Root() == nil {
		doc.CreateProcInst("xml", `version="1.0" encoding="utf-8"`)
	}

	for i, replacement := range f.Replace {
		value, err := f.LookupConfigurationValue(replacement)
		if err != nil {
			return err
		}

		// If this is the first item and there is no root element, create that root now and apply
		// it for future use.
		if i == 0 && doc.Root() == nil {
			parts := strings.SplitN(replacement.Match, ".", 2)
			doc.SetRoot(doc.CreateElement(parts[0]))
		}

		path := "./" + strings.ReplaceAll(replacement.Match, ".", "/")

		// If we're not doing a wildcard replacement go ahead and create the
		// missing element if we cannot find it yet.
		if !strings.Contains(path, "*") {
			parts := strings.Split(replacement.Match, ".")

			// Set the initial element to be the root element, and then work from there.
			element := doc.Root()

			// Iterate over the path to create the required structure for the given element's path.
			// This does not set a value, only ensures that the base structure exists. We start at index
			// 1 because an XML document can only contain a single root element, and from there we'll
			// work our way down the chain.
			for _, tag := range parts[1:] {
				if e := element.FindElement(tag); e == nil {
					element = element.CreateElement(tag)
				} else {
					element = e
				}
			}
		}

		// Iterate over the elements we found and update their values.
		for _, element := range doc.FindElements(path) {
			if xmlValueMatchRegex.MatchString(value) {
				k := xmlValueMatchRegex.ReplaceAllString(value, "$1")
				v := xmlValueMatchRegex.ReplaceAllString(value, "$2")

				element.CreateAttr(k, v)
			} else {
				element.SetText(value)
			}
		}
	}

	// Ensure the XML is indented properly.
	doc.Indent(2)

	var buf bytes.Buffer
	if _, err := doc.WriteTo(&buf); err != nil {
		return err
	}
	return rewrite(file, buf.Bytes())
}

// Parses an ini file.
func (f *ConfigurationFile) parseIniFile(file ufs.File) error {
	cfg, err := loadINIDocument(io.LimitReader(file, maxConfigFileSize))
	if err != nil {
		return err
	}

	for _, replacement := range f.Replace {
		var (
			path         []string
			bracketDepth int
			v            []int32
		)
		for _, c := range replacement.Match {
			switch c {
			case '[':
				bracketDepth++
			case ']':
				bracketDepth--
			case '.':
				if bracketDepth > 0 || len(path) == 1 {
					v = append(v, c)
					continue
				}
				path = append(path, string(v))
				v = v[:0]
			default:
				v = append(v, c)
			}
		}
		path = append(path, string(v))

		value, err := f.LookupConfigurationValue(replacement)
		if err != nil {
			return err
		}

		k := path[0]
		section := ""
		// Passing a key of foo.bar will look for "bar" in the "[foo]" section of the file.
		if len(path) == 2 {
			k = path[1]
			section = path[0]
		}

		if err := cfg.set(section, k, value); err != nil {
			return err
		}
	}

	return rewrite(file, cfg.bytes())
}

// Parses a json file updating any matching key/value pairs. If a match is not found, the
// value is set regardless in the file.
func (f *ConfigurationFile) parseJsonFile(file ufs.File) error {
	b, err := io.ReadAll(io.LimitReader(file, maxConfigFileSize))
	if err != nil {
		return err
	}

	data, err := f.IterateOverJson(b)
	if err != nil {
		return err
	}

	return rewrite(file, data.BytesIndent("", "    "))
}

// Parses a yaml file and updates any matching key/value pairs before persisting
// it back to the disk.
func (f *ConfigurationFile) parseYamlFile(file ufs.File) error {
	b, err := io.ReadAll(io.LimitReader(file, maxConfigFileSize))
	if err != nil {
		return err
	}

	i := make(map[string]interface{})
	if err := yaml.Unmarshal(b, &i); err != nil {
		return err
	}

	var root interface{}
	if i != nil {
		root = dyno.ConvertMapI2MapS(i)
	}
	data, err := f.replaceConfigurationValues(gabs.Wrap(root))
	if err != nil {
		return err
	}

	// Marshal the updated data into YAML before saving it back to disk.
	marshaled, err := yaml.Marshal(data.Data())
	if err != nil {
		return err
	}

	return rewrite(file, marshaled)
}

// Parses a text file using basic find and replace. This is a highly inefficient method of
// scanning a file and performing a replacement. You should attempt to use anything other
// than this function where possible.
func (f *ConfigurationFile) parseTextFile(file ufs.File) error {
	b := bytes.NewBuffer(nil)
	s := bufio.NewScanner(io.LimitReader(file, maxConfigFileSize))
	s.Buffer(make([]byte, 0, 64*1024), maxTextScanTokenSize)
	var replaced bool
	for s.Scan() {
		line := s.Bytes()
		replaced = false
		for _, replace := range f.Replace {
			// If this line doesn't match what we expect for the replacement, move on to the next
			// line. Otherwise, update the line to have the replacement value.
			if !bytes.HasPrefix(line, []byte(replace.Match)) {
				continue
			}
			b.Write(replace.ReplaceWith.Bytes())
			replaced = true
		}
		if !replaced {
			b.Write(line)
		}
		b.WriteByte('\n')
		// Apply the same limit to the result.
		if b.Len() > maxConfigFileSize {
			return errors.Errorf("parser: refusing to write configuration file %q: result exceeds limit of %d bytes", file.Name(), maxConfigFileSize)
		}
	}
	if err := s.Err(); err != nil {
		return errors.Wrap(err, "parser: failed to scan text file for configuration update")
	}

	return rewrite(file, b.Bytes())
}

// decodePropertiesSurrogates handles UTF-16 pairs unsupported by the properties lexer.
func decodePropertiesSurrogates(b []byte) []byte {
	var joined bytes.Buffer
	lineStart := true
	for i := 0; i < len(b); i++ {
		if lineStart {
			end := bytes.IndexAny(b[i:], "\r\n")
			if end < 0 {
				end = len(b) - i
			}
			line := bytes.TrimLeft(b[i:i+end], " \t\f")
			if len(line) > 0 && (line[0] == '#' || line[0] == '!') {
				joined.Write(b[i : i+end])
				i += end - 1
				lineStart = false
				continue
			}
		}
		lineStart = b[i] == '\n' || b[i] == '\r'
		if b[i] == '\\' && i+1 < len(b) {
			if b[i+1] == '\r' || b[i+1] == '\n' {
				i++
				if b[i] == '\r' && i+1 < len(b) && b[i+1] == '\n' {
					i++
				}
				for i+1 < len(b) && strings.ContainsRune(" \t\f", rune(b[i+1])) {
					i++
				}
				lineStart = false
				continue
			}
			joined.WriteByte(b[i])
			i++
		}
		joined.WriteByte(b[i])
	}
	b = joined.Bytes()
	var out bytes.Buffer
	for i := 0; i < len(b); {
		if b[i] == '\\' && i+1 < len(b) {
			if i+12 <= len(b) && b[i+1] == 'u' && b[i+6] == '\\' && b[i+7] == 'u' {
				hi, errHi := strconv.ParseUint(string(b[i+2:i+6]), 16, 16)
				lo, errLo := strconv.ParseUint(string(b[i+8:i+12]), 16, 16)
				if errHi == nil && errLo == nil && hi >= 0xd800 && hi <= 0xdbff && lo >= 0xdc00 && lo <= 0xdfff {
					out.WriteRune(utf16.DecodeRune(rune(hi), rune(lo)))
					i += 12
					continue
				}
			}
			out.Write(b[i : i+2])
			i += 2
			continue
		}
		out.WriteByte(b[i])
		i++
	}
	return out.Bytes()
}

// parsePropertiesFile parses a properties file and updates the values within it
// to match those that are passed. Once completed the new file is written to the
// disk. This will cause comments not present at the head of the file to be
// removed unfortunately.
//
// Any UTF-8 value will be written back to the disk as their escaped value rather
// than the raw value There is no winning with this logic. This fixes a bug where
// users with hand rolled UTF-8 escape sequences would have all sorts of pain in
// their configurations because we were writing the UTF-8 literal characters which
// their games could not actually handle.
//
// However, by adding this fix to only store the escaped UTF-8 sequence we
// unwittingly introduced a "regression" that causes _other_ games to have issues
// because they can only handle the unescaped representations. I cannot think of
// a simple approach to this problem that doesn't just lead to more complicated
// cases and problems.
//
// So, if your game cannot handle parsing UTF-8 sequences that are escaped into
// the string, well, sucks. There are fewer of those games than there are games
// that have issues parsing the raw UTF-8 sequence into a string? Also how does
// one really know what the user intended at this point? We'd need to know if
// the value was escaped or not to begin with before setting it, which I suppose
// can work but jesus that is going to be some annoyingly complicated logic?
//
// @see https://github.com/pterodactyl/panel/issues/2308 (original)
// @see https://github.com/pterodactyl/panel/issues/3009 ("bug" introduced as result)
func (f *ConfigurationFile) parsePropertiesFile(file ufs.File) error {
	b, err := io.ReadAll(io.LimitReader(file, maxConfigFileSize))
	if err != nil {
		return err
	}

	s := bytes.NewBuffer(nil)
	scanner := bufio.NewScanner(bytes.NewReader(b))
	// Scan until we hit a line that is not a comment that actually has content
	// on it. Keep appending the comments until that time.
	for scanner.Scan() {
		text := scanner.Bytes()
		if len(text) > 0 && text[0] != '#' {
			break
		}
		s.Write(text)
		s.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return errors.WithStackIf(err)
	}

	p, err := properties.Load(decodePropertiesSurrogates(b), properties.UTF8)
	if err != nil {
		return errors.Wrap(err, "parser: could not load properties file for configuration update")
	}

	// Replace any values that need to be replaced.
	for _, replace := range f.Replace {
		data, err := f.LookupConfigurationValue(replace)
		if err != nil {
			return errors.Wrap(err, "parser: failed to lookup configuration value")
		}

		v, ok := p.Get(replace.Match)
		// Don't attempt to replace the value if we're looking for a specific value and
		// it does not match. If there was no match at all in the file for this key but
		// we're doing an IfValue match, do nothing.
		if replace.IfValue != "" && (!ok || (ok && v != replace.IfValue)) {
			continue
		}

		if _, _, err := p.Set(replace.Match, data); err != nil {
			return errors.Wrap(err, "parser: failed to set replacement value")
		}
	}

	// Add the new file content to the string builder.
	for _, key := range p.Keys() {
		value, ok := p.Get(key)
		if !ok {
			continue
		}
		// This escape is intentional!
		//
		// See the docblock for this function for more details, do not change this
		// or you'll cause a flood of new issue reports no one wants to deal with.
		start := 0
		for i, r := range key {
			if r <= 0xffff {
				continue
			}
			s.WriteString(key[start:i])
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(s, `\u%04x\u%04x`, hi, lo)
			start = i + len(string(r))
		}
		s.WriteString(key[start:] + "=")
		start = 0
		for i, r := range value {
			if r <= 0xffff {
				continue
			}
			quoted := strconv.QuoteToASCII(value[start:i])
			s.WriteString(quoted[1 : len(quoted)-1])
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(s, `\u%04x\u%04x`, hi, lo)
			start = i + len(string(r))
		}
		quoted := strconv.QuoteToASCII(value[start:])
		s.WriteString(quoted[1 : len(quoted)-1])
		s.WriteByte('\n')
	}

	return rewrite(file, s.Bytes())
}

// contentReplacer replaces a file's contents, reserving disk space before
// changing the existing bytes.
type contentReplacer interface {
	ReplaceContents(data []byte) error
}

// rewrite replaces the contents of file with data. The original contents are
// left in place if data is too large or would not fit in the disk space the
// file may use. The file is overwritten in place and only then shortened, so a
// failed write cannot leave it empty.
func rewrite(file ufs.File, data []byte) error {
	if len(data) > maxConfigFileSize {
		return errors.Errorf("parser: refusing to write configuration file %q: result exceeds limit of %d bytes", file.Name(), maxConfigFileSize)
	}
	if c, ok := file.(contentReplacer); ok {
		if err := c.ReplaceContents(data); err != nil {
			return errors.Wrap(err, "parser: failed to write configuration file to disk")
		}
		return nil
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	n, err := file.Write(data)
	if err != nil {
		return errors.Wrap(err, "parser: failed to write configuration file to disk")
	}
	if err := file.Truncate(int64(n)); err != nil {
		return err
	}
	return nil
}
