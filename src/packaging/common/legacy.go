// Rename-bridge glue: the executable was called shipper before it was quesma-shipper. Delete this
// file once no install older than the rename remains.
package common

import "path/filepath"

// FormerExecutable is the pre-rename binary name.
const FormerExecutable = "shipper"

// RenamedExecutable maps a binary installed under the former name to its renamed sibling.
func RenamedExecutable(exe string) (string, bool) {
	if filepath.Base(exe) != FormerExecutable {
		return "", false
	}
	return filepath.Join(filepath.Dir(exe), "quesma-shipper"), true
}
