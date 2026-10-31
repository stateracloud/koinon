// SPDX-License-Identifier: Apache-2.0

package router

// SecretColumnNames lists the table and column of every envelope the router reads secrets from.
func SecretColumnNames() [][2]string {
	out := make([][2]string, 0, len(secretColumns))
	for _, c := range secretColumns {
		out = append(out, [2]string{c.table, c.column})
	}
	return out
}

// QueueCount is the number of live destination queues.
func (r *Router) QueueCount() int {
	r.qmu.Lock()
	defer r.qmu.Unlock()
	return len(r.queues)
}
