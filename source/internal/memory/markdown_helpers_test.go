package memory

import (
	"context"
	"io"
)

func decodeNode(r io.Reader) (Node, error) {
	return decodeNodeContext(context.Background(), r, 0)
}
