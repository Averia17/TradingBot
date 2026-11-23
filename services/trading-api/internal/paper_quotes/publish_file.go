package paper_quotes

import (
	"crypto/rand"
	"errors"
	"os"
)

// PublishNewFile writes complete bytes under name without replacing a file.
// A conflicting name returns an error matching os.ErrExist.
func PublishNewFile(root *os.Root, name string, body []byte) error {
	tmpName := ".paper-quote-" + rand.Text()
	tmp, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmpName)
	_, writeErr := tmp.Write(body)
	syncErr := tmp.Sync()
	if err := errors.Join(writeErr, syncErr, tmp.Close()); err != nil {
		return err
	}
	return root.Link(tmpName, name)
}
