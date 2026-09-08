package app

import "strings"

type Severity int

const (
	SevDim Severity = iota
	SevOK
	SevWarn
	SevFail
)

type Row struct {
	Sev    Severity
	Label  string
	Detail string
	Fix    string
	Rollup bool
	Sub    bool
	Brief  string
	Name   bool
	Tag    string
}

func (r Row) Head() string {
	if r.Tag == "" {
		return r.Label
	}
	return r.Label + " " + r.Tag
}

func Plain(s string) string { return strings.ReplaceAll(s, "`", "") }

type Section struct {
	Title string
	Rows  []Row
}

type Report struct {
	Sections         []Section
	Update           UpdateStatus
	Organization     string
	Endpoint         string
	AgentsCollecting int
	FilesFound       int
}

func (r *Report) Issues() (out []string, fails int) {
	for _, sec := range r.Sections {
		for _, row := range sec.Rows {
			if row.Sev == SevFail {
				fails++
			}
			if row.Sev == SevFail || (row.Sev == SevWarn && !row.Rollup) {
				b := row.Brief
				if b == "" {
					b = strings.TrimSpace(row.Label)
				}
				out = append(out, b)
			}
		}
	}
	if r.Update.State == "available" {
		out = append(out, r.Update.Latest+" available")
	}
	return out, fails
}
