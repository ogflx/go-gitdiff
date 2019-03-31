package gitdiff

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Parse parses a patch with changes for one or more files. Any content
// preceding the first file header is ignored. If an error occurs while
// parsing, files will contain all files parsed before the error.
func Parse(r io.Reader) ([]*File, error) {
	p := &parser{r: bufio.NewReader(r)}
	if err := p.Next(); err != nil {
		if err == io.EOF {
			return nil, nil
		}
		return nil, err
	}

	var files []*File
	for {
		file, err := p.ParseNextFileHeader()
		if err != nil {
			return files, err
		}
		if file == nil {
			break
		}

		for _, fn := range []func(*File) (int, error){
			p.ParseTextFragments,
			p.ParseBinaryFragment,
		} {
			n, err := fn(file)
			if err != nil {
				return files, err
			}
			if n > 0 {
				break
			}
		}
		// file has fragment(s) from above or the patch is empty or invalid
		files = append(files, file)
	}

	return files, nil
}

// TODO(bkeyes): consider exporting the parser type with configuration
// this would enable OID validation, p-value guessing, and prefix stripping
// by allowing users to set or override defaults

// parser invariants:
// - methods that parse objects:
//     - start with the parser on the first line of the first object
//     - if returning nil, do not advance
//     - if returning an error, do not advance past the object
//     - if returning an object, advance to the first line after the object
// - any exported parsing methods must initialize the parser by calling Next()

type parser struct {
	r *bufio.Reader

	eof    bool
	lineno int64
	lines  [3]string
}

// ParseNextFileHeader finds and parses the next file header in the stream. It
// returns nil if no headers are found before the end of the stream.
func (p *parser) ParseNextFileHeader() (*File, error) {
	var file *File
	for {
		// check for disconnected fragment headers (corrupt patch)
		frag, err := p.ParseTextFragmentHeader()
		if err != nil {
			// not a valid header, nothing to worry about
			goto NextLine
		}
		if frag != nil {
			return nil, p.Errorf(-1, "patch fragment without file header: %s", frag.Header())
		}

		// check for a git-generated patch
		file, err = p.ParseGitFileHeader()
		if err != nil {
			return nil, err
		}
		if file != nil {
			return file, nil
		}

		// check for a "traditional" patch
		file, err = p.ParseTraditionalFileHeader()
		if err != nil {
			return nil, err
		}
		if file != nil {
			return file, nil
		}

	NextLine:
		if err := p.Next(); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
	}
	return nil, nil
}

// ParseTextFragments parses text fragments until the next file header or the
// end of the stream and attaches them to the given file. It returns the number
// of fragments that were added.
func (p *parser) ParseTextFragments(f *File) (n int, err error) {
	for {
		frag, err := p.ParseTextFragmentHeader()
		if err != nil {
			return n, err
		}
		if frag == nil {
			return n, nil
		}

		if f.IsNew && frag.OldLines > 0 {
			return n, p.Errorf(-1, "new file depends on old contents")
		}
		if f.IsDelete && frag.NewLines > 0 {
			return n, p.Errorf(-1, "deleted file still has contents")
		}

		if err := p.ParseTextChunk(frag); err != nil {
			return n, nil
		}

		f.Fragments = append(f.Fragments, frag)
		n++
	}
}

// Next advances the parser by one line. It returns any error encountered while
// reading the line, including io.EOF when the end of stream is reached.
func (p *parser) Next() error {
	if p.eof {
		return io.EOF
	}

	if p.lineno == 0 {
		// on first call to next, need to shift in all lines
		for i := 0; i < len(p.lines)-1; i++ {
			if err := p.shiftLines(); err != nil && err != io.EOF {
				return err
			}
		}
	}

	err := p.shiftLines()
	if err != nil && err != io.EOF {
		return err
	}

	p.lineno++
	if p.lines[0] == "" {
		p.eof = true
		return io.EOF
	}
	return nil
}

func (p *parser) shiftLines() (err error) {
	for i := 0; i < len(p.lines)-1; i++ {
		p.lines[i] = p.lines[i+1]
	}
	p.lines[len(p.lines)-1], err = p.r.ReadString('\n')
	return
}

// Line returns a line from the parser without advancing it. A delta of 0
// returns the current line, while higher deltas return read-ahead lines. It
// returns an empty string if the delta is higher than the available lines,
// either because of the buffer size or because the parser reached the end of
// the input. Valid lines always contain at least a newline character.
func (p *parser) Line(delta uint) string {
	return p.lines[delta]
}

// Errorf generates an error and appends the current line information.
func (p *parser) Errorf(delta int64, msg string, args ...interface{}) error {
	return fmt.Errorf("gitdiff: line %d: %s", p.lineno+delta, fmt.Sprintf(msg, args...))
}

func (p *parser) ParseTextFragmentHeader() (*Fragment, error) {
	const (
		startMark = "@@ -"
		endMark   = " @@"
	)

	if !strings.HasPrefix(p.Line(0), startMark) {
		return nil, nil
	}

	parts := strings.SplitAfterN(p.Line(0), endMark, 2)
	if len(parts) < 2 {
		return nil, p.Errorf(0, "invalid fragment header")
	}

	f := &Fragment{}
	f.Comment = strings.TrimSpace(parts[1])

	header := parts[0][len(startMark) : len(parts[0])-len(endMark)]
	ranges := strings.Split(header, " +")
	if len(ranges) != 2 {
		return nil, p.Errorf(0, "invalid fragment header")
	}

	var err error
	if f.OldPosition, f.OldLines, err = parseRange(ranges[0]); err != nil {
		return nil, p.Errorf(0, "invalid fragment header: %v", err)
	}
	if f.NewPosition, f.NewLines, err = parseRange(ranges[1]); err != nil {
		return nil, p.Errorf(0, "invalid fragment header: %v", err)
	}

	if err := p.Next(); err != nil && err != io.EOF {
		return nil, err
	}
	return f, nil
}

func (p *parser) ParseTextChunk(frag *Fragment) error {
	if p.Line(0) == "" {
		return p.Errorf(0, "no content following fragment header")
	}

	oldLines, newLines := frag.OldLines, frag.NewLines
	for oldLines > 0 || newLines > 0 {
		line := p.Line(0)
		switch line[0] {
		case '\n':
			fallthrough // newer GNU diff versions create empty context lines
		case ' ':
			oldLines--
			newLines--
			if frag.LinesAdded == 0 && frag.LinesDeleted == 0 {
				frag.LeadingContext++
			} else {
				frag.TrailingContext++
			}
			frag.Lines = append(frag.Lines, FragmentLine{OpContext, line[1:]})
		case '-':
			frag.LinesDeleted++
			oldLines--
			frag.Lines = append(frag.Lines, FragmentLine{OpDelete, line[1:]})
		case '+':
			frag.LinesAdded++
			newLines--
			frag.Lines = append(frag.Lines, FragmentLine{OpAdd, line[1:]})
		default:
			// this could be "\ No newline at end of file", which we allow
			// only check the prefix because the text changes by locale
			// git also asserts that any translation is at least 12 characters
			if len(line) >= 12 && strings.HasPrefix("\\ ", line) {
				break
			}
			return p.Errorf(0, "invalid fragment line")
		}

		if err := p.Next(); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}

	if oldLines != 0 || newLines != 0 {
		return p.Errorf(0, "invalid fragment: remaining lines: %d old, %d new", oldLines, newLines)
	}
	return nil
}

func (p *parser) ParseBinaryFragment(f *File) (n int, err error) {
	panic("TODO(bkeyes): unimplemented")
}

func parseRange(s string) (start int64, end int64, err error) {
	parts := strings.SplitN(s, ",", 2)

	if start, err = strconv.ParseInt(parts[0], 10, 64); err != nil {
		nerr := err.(*strconv.NumError)
		return 0, 0, fmt.Errorf("bad start of range: %s: %v", parts[0], nerr.Err)
	}

	if len(parts) > 1 {
		if end, err = strconv.ParseInt(parts[1], 10, 64); err != nil {
			nerr := err.(*strconv.NumError)
			return 0, 0, fmt.Errorf("bad end of range: %s: %v", parts[1], nerr.Err)
		}
	} else {
		end = 1
	}

	return
}
