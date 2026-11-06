package windows

// taskQueryPhases retains only complete, allowlisted markers, never arbitrary command output.
type taskQueryPhases struct {
	line     [16]byte
	n        int
	overflow bool
	last     string
}

func (p *taskQueryPhases) Write(data []byte) (int, error) {
	for _, b := range data {
		if b == '\n' {
			if !p.overflow {
				line := p.line[:p.n]
				if len(line) > 0 && line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
				switch string(line) {
				case "COM", "connect", "folder", "task":
					p.last = string(line)
				}
			}
			p.n, p.overflow = 0, false
		} else if p.n < len(p.line) && !p.overflow {
			p.line[p.n] = b
			p.n++
		} else {
			p.overflow = true
		}
	}
	return len(data), nil
}

func (p *taskQueryPhases) phase() string {
	if p.last == "" {
		return "PowerShell startup"
	}
	return p.last
}
