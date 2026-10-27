package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/ini.v1"
)

func rewriteINI(t *testing.T, content string, replacements ...ConfigurationFileReplacement) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "Game.ini")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	cf := ConfigurationFile{Replace: replacements}
	err = cf.parseIniFile(file)
	output, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	return string(output), err
}

func TestINIRewritePreservesRepeatedEntriesInOrder(t *testing.T) {
	input := "; crafting overrides\n[ServerSettings]\nSessionName=old\n" +
		"ConfigOverrideItemCraftingCosts=(ItemClassString=\"First\",BaseCraftingResourceRequirements=())\n" +
		"OtherSetting=keep\n" +
		"ConfigOverrideItemCraftingCosts=(ItemClassString=\"Second\",BaseCraftingResourceRequirements=())\n" +
		"ConfigOverrideItemCraftingCosts=(ItemClassString=\"First\",BaseCraftingResourceRequirements=())\n" +
		"LastSetting=keep\n" +
		"ConfigOverrideItemCraftingCosts=(ItemClassString=\"Third\",BaseCraftingResourceRequirements=())\n"
	output, err := rewriteINI(t, input, replacement(t, `{"match":"ServerSettings.SessionName","replace_with":"new"}`))
	require.NoError(t, err)
	require.Equal(t, strings.Replace(input, "SessionName=old", "SessionName = new", 1), output)
}

func TestINIRewriteReplacesRepeatedTargetsWithoutMovingThem(t *testing.T) {
	input := "[Settings]\nvalue=first\nother=keep\nvalue=second\nvalue=second\n[Other]\nvalue=untouched\n"
	output, err := rewriteINI(t, input, replacement(t, `{"match":"Settings.value","replace_with":"new;value"}`))
	require.NoError(t, err)
	require.Equal(t, "[Settings]\nvalue = `new;value`\nother=keep\nvalue = `new;value`\nvalue = `new;value`\n[Other]\nvalue=untouched\n", output)
}

func TestINIRewritePreservesMultilineRecordBoundaries(t *testing.T) {
	for _, multiline := range []string{
		"description=\"\"\"\n[Settings]\nvalue=inside\n\"\"\"\n",
		"description=\"\"\"first\n[Settings]\nvalue=inside\nlast\"\"\"\n",
		"description=`first\n[Settings]\nvalue=inside\nlast`\n",
		"description=first\\\nvalue=inside\\\nlast\n",
	} {
		t.Run(multiline, func(t *testing.T) {
			input := "[Settings]\n" + multiline + "value=outside\n"
			output, err := rewriteINI(t, input, replacement(t, `{"match":"Settings.value","replace_with":"new"}`))
			require.NoError(t, err)
			require.Equal(t, "[Settings]\n"+multiline+"value = new\n", output)
		})
	}
}

func TestINIRewriteHandlesSupportedByteOrderMarks(t *testing.T) {
	for _, bom := range []string{"\ufeff", "\xff\xfe", "\xfe\xff"} {
		t.Run(bom, func(t *testing.T) {
			output, err := rewriteINI(t, bom+"[Settings]\nvalue=old\n",
				replacement(t, `{"match":"Settings.value","replace_with":"new"}`))
			require.NoError(t, err)
			require.Equal(t, bom+"[Settings]\nvalue = new\n", output)
			output, err = rewriteINI(t, bom+"[Settings]\nvalue=keep\n",
				replacement(t, `{"match":"root","replace_with":"new"}`))
			require.NoError(t, err)
			require.Equal(t, bom+"root = new\n[Settings]\nvalue=keep\n", output)
		})
	}
}

func TestINIRewriteTargetsQuotedAndAutoIncrementKeys(t *testing.T) {
	input := "[Settings]\n`key:with=delimiters`=first\n\"key:with=delimiters\"=second\n-=first\n-=second\n"
	output, err := rewriteINI(t, input,
		replacement(t, `{"match":"Settings.key:with=delimiters","replace_with":"new"}`),
		replacement(t, `{"match":"Settings.#2","replace_with":"changed"}`))
	require.NoError(t, err)
	require.Equal(t, "[Settings]\n`key:with=delimiters` = new\n`key:with=delimiters` = new\n-=first\n-  = changed\n", output)
}

func TestINIRewriteAddsMissingEntries(t *testing.T) {
	for _, input := range []string{"", "root=keep", "[Settings]\nother=keep", "[Other]\nother=keep\n"} {
		t.Run(input, func(t *testing.T) {
			output, err := rewriteINI(t, input,
				replacement(t, `{"match":"root","replace_with":"new-root"}`),
				replacement(t, `{"match":"Settings.value","replace_with":"new"}`),
				replacement(t, `{"match":"Settings.second","replace_with":"another"}`))
			require.NoError(t, err)
			cfg, err := ini.Load([]byte(output))
			require.NoError(t, err)
			require.Equal(t, "new-root", cfg.Section("").Key("root").Value())
			require.Equal(t, "new", cfg.Section("Settings").Key("value").Value())
			require.Equal(t, "another", cfg.Section("Settings").Key("second").Value())
			if strings.Contains(input, "other=keep") {
				require.Contains(t, output, "other=keep")
			}
		})
	}
}

func TestINIRewritePreservesQuotedKeysAndRepeatedSections(t *testing.T) {
	input := "\ufeff[Settings]\r\n`key:with=delimiters`=first\r\n\"key:with=delimiters\"=second\r\nvalue=first\r\n[Other]\r\nvalue=keep\r\n[Settings]\r\nvalue=second"
	output, err := rewriteINI(t, input, replacement(t, `{"match":"Settings.value","replace_with":"new"}`))
	require.NoError(t, err)
	require.Equal(t, strings.ReplaceAll(strings.ReplaceAll(input, "value=first\r\n", "value = new\n"), "value=second", "value = new\n"), output)
}

func TestINIRewriteRejectsInvalidInputWithoutChangingFile(t *testing.T) {
	for _, input := range []string{"[unclosed\nvalue=old\n", "=invalid\n", "value=\"\"\"unclosed\n"} {
		t.Run(input, func(t *testing.T) {
			output, err := rewriteINI(t, input, replacement(t, `{"match":"value","replace_with":"new"}`))
			require.Error(t, err)
			require.Equal(t, input, output)
		})
	}
}

func TestINIRewriteRejectsEmptyTargetWithoutChangingFile(t *testing.T) {
	input := "; comment\nvalue=keep\n"
	var r ConfigurationFileReplacement
	require.NoError(t, json.Unmarshal([]byte(`{"match":"","replace_with":"new"}`), &r))
	output, err := rewriteINI(t, input, r)
	require.Error(t, err)
	require.Equal(t, input, output)
}
