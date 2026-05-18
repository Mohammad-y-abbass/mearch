package scanner

// ScanError represents a non-fatal error encountered while walking a single
// path during a scan.
type ScanError struct {
	Path string
	Err  error
}

func (e *ScanError) Error() string {
	return "mearch/scanner: error at " + e.Path + ": " + e.Err.Error()
}

func (e *ScanError) Unwrap() error {
	return e.Err
}

// ScanErrors is a slice of ScanError that implements the error interface.
//
// Returned from Scan() when non-fatal errors were encountered. The scan
// result is still usable — ScanErrors indicates incomplete coverage, not
// a failed scan.
//
// Usage:
//
//	files, err := s.Scan()
//	var scanErrs scanner.ScanErrors
//	if errors.As(err, &scanErrs) {
//	    for _, e := range scanErrs {
//	        log.Printf("skipped %s: %v", e.Path, e.Err)
//	    }
//	}
type ScanErrors []*ScanError

func (se ScanErrors) Error() string {
	if len(se) == 1 {
		return se[0].Error()
	}
	return se[0].Error() + " (and " + itoa(len(se)-1) + " more scan errors)"
}
