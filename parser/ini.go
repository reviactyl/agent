package parser

import (
	"bytes"
	"io"
	"strconv"
	"strings"

	"gopkg.in/ini.v1"
)

type iniRecord struct {
	section string
	key     string
	text    string
}

type iniDocument struct {
	bom     string
	records []iniRecord
}

func loadINIDocument(reader io.Reader) (*iniDocument, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if _, err := ini.Load(data); err != nil {
		return nil, err
	}

	document := &iniDocument{}
	content := string(data)
	for _, bom := range []string{"\ufeff", "\xff\xfe", "\xfe\xff"} {
		if strings.HasPrefix(content, bom) {
			document.bom = bom
			content = strings.TrimPrefix(content, bom)
			break
		}
	}
	section, auto := ini.DefaultSection, 0
	lines := strings.SplitAfter(content, "\n")
	for i := 0; i < len(lines); i++ {
		if lines[i] == "" {
			continue
		}
		start := i
		line := strings.TrimSpace(lines[i])
		record := iniRecord{section: section, text: lines[i]}
		switch {
		case line == "", strings.HasPrefix(line, "#"), strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "["):
			section = line[1:strings.LastIndexByte(line, ']')]
			if section == "" {
				section = ini.DefaultSection
			}
			record.section = section
			auto = 0
		default:
			key, value := iniKeyValue(line)
			if key == "-" {
				auto++
				key = "#" + strconv.Itoa(auto)
			}
			record.key = key
			// Keep continuation and quoted multiline values in their original record.
			quote := ""
			if strings.HasPrefix(value, `"""`) {
				quote = `"""`
			} else if strings.HasPrefix(value, "`") {
				quote = "`"
			}
			if quote != "" {
				if !strings.Contains(value[len(quote):], quote) {
					for i+1 < len(lines) {
						i++
						if end := strings.LastIndex(lines[i], quote); end >= 0 && !strings.HasSuffix(strings.TrimSpace(lines[i][end+len(quote):]), `\`) {
							break
						}
					}
				}
			} else if strings.HasSuffix(value, `\`) {
				for i+1 < len(lines) {
					i++
					if !strings.HasSuffix(strings.TrimSpace(lines[i]), `\`) {
						break
					}
				}
			}
			record.text = strings.Join(lines[start:i+1], "")
		}
		document.records = append(document.records, record)
	}
	return document, nil
}

// The complete document is validated before identifying record boundaries.
func iniKeyValue(line string) (string, string) {
	quote := ""
	if strings.HasPrefix(line, `"""`) {
		quote = `"""`
	} else if strings.HasPrefix(line, `"`) {
		quote = `"`
	} else if strings.HasPrefix(line, "`") {
		quote = "`"
	}
	if quote != "" {
		end := len(quote) + strings.Index(line[len(quote):], quote)
		offset := end + len(quote)
		delimiter := offset + strings.IndexAny(line[offset:], "=:")
		return strings.TrimSpace(line[len(quote):end]), strings.TrimSpace(line[delimiter+1:])
	}
	delimiter := strings.IndexAny(line, "=:")
	return strings.TrimSpace(line[:delimiter]), strings.TrimSpace(line[delimiter+1:])
}

func (d *iniDocument) set(section, key, value string) error {
	if section == "" {
		section = ini.DefaultSection
	}
	found, insertAt, sectionExists := false, 0, section == ini.DefaultSection
	for i := range d.records {
		record := &d.records[i]
		if record.section != section {
			continue
		}
		insertAt, sectionExists = i+1, true
		if record.key == "" || record.key != key {
			continue
		}
		cfg, err := ini.Load([]byte(record.text))
		if err != nil {
			return err
		}
		cfg.Section("").Keys()[0].SetValue(value)
		var buf bytes.Buffer
		if _, err := cfg.WriteTo(&buf); err != nil {
			return err
		}
		record.text = buf.String()
		found = true
	}
	if found {
		return nil
	}
	cfg := ini.Empty()
	if _, err := cfg.Section("").NewKey(key, value); err != nil {
		return err
	}
	var buf bytes.Buffer
	if _, err := cfg.WriteTo(&buf); err != nil {
		return err
	}
	if !sectionExists {
		insertAt = len(d.records)
		if insertAt > 0 && !strings.HasSuffix(d.records[insertAt-1].text, "\n") {
			d.records[insertAt-1].text += "\n"
		}
		d.records = append(d.records, iniRecord{section: section, text: "[" + section + "]\n"})
		insertAt++
	}
	if insertAt > 0 && !strings.HasSuffix(d.records[insertAt-1].text, "\n") {
		d.records[insertAt-1].text += "\n"
	}
	d.records = append(d.records, iniRecord{})
	copy(d.records[insertAt+1:], d.records[insertAt:])
	d.records[insertAt] = iniRecord{section: section, key: key, text: buf.String()}
	return nil
}

func (d iniDocument) bytes() []byte {
	var buf bytes.Buffer
	buf.WriteString(d.bom)
	for _, record := range d.records {
		buf.WriteString(record.text)
	}
	return buf.Bytes()
}
