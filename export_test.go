package onet

func (c *Server) CreateProtocol(name string, t *Tree) (ProtocolInstance, error) {
	return c.overlay.CreateProtocol(name, t, NilServiceID)
}

func (c *Server) StartProtocol(name string, t *Tree) (ProtocolInstance, error) {
	return c.overlay.StartProtocol(name, t, NilServiceID)
}

func (c *Server) GetTree(id TreeID) (*Tree, bool) {
	t := c.overlay.treeCache.Get(id)
	return t, t != nil
}

func (c *Server) Overlay() *Overlay {
	return c.overlay
}

func (o *Overlay) TokenToNode(tok *Token) (*TreeNodeInstance, bool) {
	tni, ok := o.instances[tok.ID()]
	return tni, ok
}

// AddTree registers the given Tree struct in the underlying overlay.
// Useful for unit-testing only.
func (c *Server) AddTree(t *Tree) {
	c.overlay.RegisterTree(t)
}
