package main

import (
	"bytes"
	"database/sql"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"text/template"

	_ "modernc.org/sqlite"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/transform"
)

var (
	// Pre-compile regex for performance
	metaCharsetRE = regexp.MustCompile(`(?i)<meta\s+[^>]*charset\s*=\s*["']?([a-zA-Z0-9-]+)["']?`)
	safeBundleRE  = regexp.MustCompile(`[^^a-zA-Z\d-_]`)
	titleRE       = regexp.MustCompile(`(?i)<title[^>]*>([^<]+)</title>`)
	spacesRE      = regexp.MustCompile(`[\s\t\r\n]+`)
)

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
  <dict>
    <key>dashIndexFilePath</key>
    <string>Welcome.htm</string>
    <key>CFBundleIdentifier</key>
    <string>{{.BundleIdentifier}}</string>
    <key>CFBundleName</key>
    <string>{{.Basename}}</string>
    <key>DocSetPlatformFamily</key>
    <string>{{.Platform}}</string>
    <key>isDashDocset</key>
    <true/>
  </dict>
</plist>`

func usage() {
	fmt.Fprintf(os.Stderr, "usage: %s [options] [inputfile]\n", os.Args[0])
	flag.PrintDefaults()
	os.Exit(2)
}

func failOnError(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// Options options
type Options struct {
	Outdir     string
	Platform   string
	SourcePath string
}

// parseFlags handles CLI arguments and returns Options
func parseFlags() *Options {
	opts := &Options{}
	flag.StringVar(&opts.Platform, "platform", "unknown", "DocSet Platform Family")
	flag.StringVar(&opts.Outdir, "out", "./", "Output directory or file path")
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	if len(args) != 1 {
		return nil
	}
	opts.SourcePath = args[0]
	return opts
}

// SourceFilename returns source file name
func (opts *Options) SourceFilename() string {
	return filepath.Base(opts.SourcePath)
}

// Basename returns file basename
func (opts *Options) Basename() string {
	fn := opts.SourceFilename()
	ext := filepath.Ext(fn)
	return fn[0 : len(fn)-len(ext)]
}

// DocsetPath returns path to docset bundle
func (opts *Options) DocsetPath() string {
	if strings.HasSuffix(opts.Outdir, ".docset") {
		return opts.Outdir
	}
	return filepath.Join(opts.Outdir, opts.Basename()+".docset")
}

// ContentPath returns path to docset resources
func (opts *Options) ContentPath() string {
	return filepath.Join(opts.DocsetPath(), "Contents", "Resources", "Documents")
}

// DatabasePath returns path to SQLite3 database
func (opts *Options) DatabasePath() string {
	return filepath.Join(opts.DocsetPath(), "Contents", "Resources", "docSet.dsidx")
}

// PlistPath returns path to Info.plist
func (opts *Options) PlistPath() string {
	return filepath.Join(opts.DocsetPath(), "Contents", "Info.plist")
}

// BundleIdentifier returns bundle identifier of docset bundle
func (opts *Options) BundleIdentifier() string {
	return "io.ngs.documentation." + safeBundleRE.ReplaceAllString(opts.Basename(), "")
}

// WritePlist writes plist file
func (opts *Options) WritePlist() error {
	t, err := template.New("plist").Parse(plistTemplate)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, opts); err != nil {
		return err
	}

	return os.WriteFile(opts.PlistPath(), buf.Bytes(), 0644)
}

// Clean removes existing output
func (opts *Options) Clean() error {
	return os.RemoveAll(opts.DocsetPath())
}

// CreateDirectory creates directory
func (opts *Options) CreateDirectory() error {
	return os.MkdirAll(opts.ContentPath(), 0755)
}

// ExtractSource extracts source to destination
func (opts *Options) ExtractSource() error {
	source := filepath.Clean(opts.SourcePath)
	destination := filepath.Clean(opts.ContentPath())
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("hh.exe", "-decompile", destination, source)
	} else {
		cmd = exec.Command("extract_chmLib", source, destination)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func decodeToUTF8(b []byte) string {
	searchLimit := len(b)
	if searchLimit > 4096 {
		searchLimit = 4096
	}
	match := metaCharsetRE.FindSubmatch(b[:searchLimit])
	if len(match) < 2 {
		return string(b)
	}
	charsetName := strings.ToLower(string(match[1]))
	if charsetName == "utf-8" || charsetName == "utf8" {
		return string(b)
	}
	enc, err := getEncoding(charsetName)
	if err != nil {
		return string(b)
	}
	reader := transform.NewReader(bytes.NewReader(b), enc.NewDecoder())
	decodedBytes, err := io.ReadAll(reader)
	if err != nil {
		return string(b)
	}
	return string(decodedBytes)
}

func getEncoding(name string) (encoding.Encoding, error) {
	enc, err := ianaindex.MIME.Encoding(name)
	if err != nil {
		enc, err = ianaindex.IANA.Encoding(name)
	}
	return enc, err
}

// extractTitle reads the file, handles encoding, and finds the HTML title
func extractTitle(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	content := decodeToUTF8(b)
	match := titleRE.FindStringSubmatch(content)
	if len(match) >= 2 {
		title := html.UnescapeString(match[1])
		return strings.TrimSpace(spacesRE.ReplaceAllString(title, " ")), nil
	}
	return "", nil
}

// CreateDatabase creates database
func (opts *Options) CreateDatabase() error {
	os.Remove(opts.DatabasePath())

	db, err := sql.Open("sqlite", opts.DatabasePath())
	if err != nil {
		return err
	}
	defer db.Close()

	sqlStmt := `
		CREATE TABLE searchIndex(id INTEGER PRIMARY KEY, name TEXT, type TEXT, path TEXT);
		CREATE UNIQUE INDEX anchor ON searchIndex (name, type, path);
		`
	if _, err = db.Exec(sqlStmt); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("INSERT OR IGNORE INTO searchIndex(name, type, path) VALUES (?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	basePath := opts.ContentPath()

	walkFn := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".htm" || ext == ".html" {
			title, err := extractTitle(path)
			if err != nil {
				return err
			}

			if title != "" {
				relPath, err := filepath.Rel(basePath, path)
				if err != nil {
					return err
				}
				relPath = filepath.ToSlash(relPath)
				_, err = stmt.Exec(title, "Guide", relPath)
				if err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err = filepath.Walk(basePath, walkFn); err != nil {
		return err
	}
	return tx.Commit()
}

func main() {
	opts := parseFlags()
	if opts == nil {
		usage()
		return
	}
	failOnError(opts.Clean())
	failOnError(opts.CreateDirectory())
	failOnError(opts.ExtractSource())
	failOnError(opts.CreateDatabase())
	failOnError(opts.WritePlist())
}
