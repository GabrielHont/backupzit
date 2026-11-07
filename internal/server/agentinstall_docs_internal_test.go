package server

import (
	"encoding/json"
	"encoding/xml"
	"html"
	"io"
	"os"
	"strings"
	"testing"
)

func TestAgentInstallerDocumentation(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]struct {
			Examples []struct {
				Source string `json:"source"`
			} `json:"x-codeSamples"`
		}
	}
	if err := json.Unmarshal(publicOpenAPI, &doc); err != nil {
		t.Fatal(err)
	}
	guide, err := templateFS.ReadFile("templates/doc_api.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/agent-packages", "/agent/{file}"} {
		examples := doc.Paths[path]["get"].Examples
		if len(examples) != 3 {
			t.Fatalf("%s: expected Windows, DEB and RPM installation examples", path)
		}
		for _, example := range examples {
			if !strings.Contains(html.UnescapeString(string(guide)), example.Source) {
				t.Errorf("OpenAPI and HTML installation commands differ for %s", path)
			}
			if !strings.Contains(example.Source, "/agent/$") || !strings.Contains(example.Source, "SHA256_FROM_AGENT_PACKAGES") || strings.Contains(example.Source, "Bearer ") {
				t.Errorf("example must use a public download and catalog SHA-256 without exposing an API token")
			}
		}
	}
	// Assert the documented MSI property and command against the real WiX
	// source, rather than assuming the MSI accepts a CLI flag or TOKEN.
	wix, err := os.Open("../../packaging/windows/agent.wxs")
	if err != nil {
		t.Fatal(err)
	}
	defer wix.Close()
	d := xml.NewDecoder(wix)
	property, command := false, false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if start, ok := tok.(xml.StartElement); ok {
			attrs := map[string]string{}
			for _, attr := range start.Attr {
				attrs[attr.Name.Local] = attr.Value
			}
			if start.Name.Local == "Property" && attrs["Id"] == "ENROLLCODE" {
				property = attrs["Secure"] == "yes" && attrs["Hidden"] == "yes"
			}
			if start.Name.Local == "CustomAction" && attrs["Id"] == "EnrollCode" {
				command = attrs["ExeCommand"] == `enroll --if-not-enrolled --pending-ok --code "[ENROLLCODE]"`
			}
		}
	}
	if !property || !command {
		t.Fatal("installer no longer implements the documented ENROLLCODE command")
	}
	cli, err := os.ReadFile("../../cmd/backupzit-agent/service.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cli), `flag.NewFlagSet("enroll"`) || !strings.Contains(string(cli), `fs.String("code",`) {
		t.Fatal("agent no longer implements the documented enroll --code command")
	}
}
