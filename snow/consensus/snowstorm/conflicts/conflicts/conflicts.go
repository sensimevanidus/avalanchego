// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package conflicts

import (
	"errors"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/choices"
)

var (
	errInvalidTxType = errors.New("invalid tx type")
)

type Conflicts struct {
	// track the currently processing txs. Maps ID() to tx.
	txs map[[32]byte]Tx

	// transitions tracks the currently processing txIDs for a transition. Maps
	// transitionID to a set of txIDs.
	transitions map[[32]byte]ids.Set

	// track which txs are currently consuming which utxos. Maps inputID to a
	// set of txIDs.
	utxos map[[32]byte]ids.Set

	// track which txs are attempting to be restricted. Maps transitionID to a
	// set of txIDs.
	restrictions map[[32]byte]ids.Set

	// track which txs are blocking on others. Maps transitionID to a set of
	// txIDs.
	dependencies map[[32]byte]ids.Set

	// accepted tracks the set of txIDs that have been conditionally accepted,
	// but not returned as fully accepted.
	accepted ids.Set

	// track txs that have been marked as ready to accept
	acceptable []choices.Decidable

	// track txs that have been marked as ready to reject
	rejectable []choices.Decidable
}

func New() *Conflicts {
	return &Conflicts{
		txs:          make(map[[32]byte]Tx),
		transitions:  make(map[[32]byte]ids.Set),
		utxos:        make(map[[32]byte]ids.Set),
		restrictions: make(map[[32]byte]ids.Set),
		dependencies: make(map[[32]byte]ids.Set),
	}
}

// Add this tx to the conflict set. If this tx is of the correct type, this tx
// will be added to the set of processing txs. It is assumed this tx wasn't
// already processing. This will mark the consumed utxos and register a rejector
// that will be notified if a dependency of this tx was rejected.
func (c *Conflicts) Add(txIntf choices.Decidable) error {
	tx, ok := txIntf.(Tx)
	if !ok {
		return errInvalidTxType
	}

	txID := tx.ID()
	c.txs[txID.Key()] = tx

	transitionID := tx.TransitionID()
	transitionKey := transitionID.Key()
	txIDs := c.transitions[transitionKey]
	txIDs.Add(txID)
	c.transitions[transitionKey] = txIDs

	for inputKey := range tx.InputIDs() {
		spenders := c.utxos[inputKey]
		spenders.Add(txID)
		c.utxos[inputKey] = spenders
	}

	for _, restrictionID := range tx.Restrictions() {
		restrictionKey := restrictionID.Key()
		restrictors := c.restrictions[restrictionKey]
		restrictors.Add(txID)
		c.restrictions[restrictionKey] = restrictors
	}

	for _, dependencyID := range tx.Dependencies() {
		dependencyKey := dependencyID.Key()
		dependents := c.dependencies[dependencyKey]
		dependents.Add(txID)
		c.dependencies[dependencyKey] = dependents
	}
	return nil
}

// IsVirtuous checks the currently processing txs for conflicts. It is allowed
// to call this function with txs that aren't yet processing or currently
// processing txs.
func (c *Conflicts) IsVirtuous(txIntf choices.Decidable) (bool, error) {
	tx, ok := txIntf.(Tx)
	if !ok {
		return false, errInvalidTxType
	}

	txID := tx.ID()
	for inputKey := range tx.InputIDs() {
		spenders := c.utxos[inputKey]
		if numSpenders := spenders.Len(); numSpenders > 1 ||
			(numSpenders == 1 && !spenders.Contains(txID)) {
			return false, nil
		}
	}

	epoch := tx.Epoch()
	transitionID := tx.TransitionID()
	transitionKey := transitionID.Key()
	restrictors := c.restrictions[transitionKey]
	for restrictorKey := range restrictors {
		restrictor := c.txs[restrictorKey]
		restrictorEpoch := restrictor.Epoch()
		if restrictorEpoch <= epoch {
			return false, nil
		}
	}
	return true, nil
}

// Conflicts returns the collection of txs that are currently processing that
// conflict with the provided tx. It is allowed to call with function with txs
// that aren't yet processing or currently processing txs.
func (c *Conflicts) Conflicts(txIntf choices.Decidable) ([]choices.Decidable, error) {
	tx, ok := txIntf.(Tx)
	if !ok {
		return nil, errInvalidTxType
	}

	txID := tx.ID()
	var conflictSet ids.Set
	conflictSet.Add(txID)

	conflicts := []choices.Decidable(nil)
	for inputKey := range tx.InputIDs() {
		spenders := c.utxos[inputKey]
		for spenderKey := range spenders {
			spenderID := ids.NewID(spenderKey)
			if conflictSet.Contains(spenderID) {
				continue
			}
			conflictSet.Add(spenderID)
			conflicts = append(conflicts, c.txs[spenderKey])
		}
	}

	epoch := tx.Epoch()
	transitionID := tx.TransitionID()
	transitionKey := transitionID.Key()
	restrictors := c.restrictions[transitionKey]
	for restrictorKey := range restrictors {
		restrictorID := ids.NewID(restrictorKey)
		if conflictSet.Contains(restrictorID) {
			continue
		}

		restrictor := c.txs[restrictorKey]
		restrictorEpoch := restrictor.Epoch()
		if restrictorEpoch > epoch {
			continue
		}

		conflictSet.Add(restrictorID)
		conflicts = append(conflicts, restrictor)
	}
	return conflicts, nil
}

// Accept notifies this conflict manager that a tx has been conditionally
// accepted. This means that assuming all the txs this tx depends on are
// accepted, then this tx should be accepted as well. This
func (c *Conflicts) Accept(txID ids.ID) {
	tx, exists := c.txs[txID.Key()]
	if !exists {
		return
	}
	c.accepted.Add(txID)

	acceptable := true
	for _, dependency := range tx.Dependencies() {
		if !c.accepted.Contains(dependency) {
			acceptable = false
		}
	}
	if acceptable {
		c.acceptable = append(c.acceptable, tx)
	}
}

func (c *Conflicts) Updateable() ([]choices.Decidable, []choices.Decidable) {
	acceptable := c.acceptable
	c.acceptable = nil

	// rejected tracks the set of txIDs that have been rejected but not yet
	// removed from the graph.
	rejected := ids.Set{}

	for _, tx := range acceptable {
		txID := tx.ID()

		tx := tx.(Tx)
		for inputKey := range tx.InputIDs() {
			spenders := c.utxos[inputKey]
			spenders.Remove(txID)
			if spenders.Len() == 0 {
				delete(c.utxos, inputKey)
			} else {
				c.utxos[inputKey] = spenders
			}
		}

		delete(c.txs, txID.Key())
		c.pendingAccept.Fulfill(txID)
		c.pendingReject.Abandon(txID)

		// Conflicts should never return an error, as the type has already been
		// asserted.
		conflicts, _ := c.Conflicts(tx)
		for _, conflict := range conflicts {
			conflictID := conflict.ID()
			if rejected.Contains(conflictID) {
				continue
			}
			rejected.Add(conflictID)
			c.rejectable = append(c.rejectable, conflict)
		}
	}

	rejectable := c.rejectable
	c.rejectable = nil
	for _, tx := range rejectable {
		txID := tx.ID()

		tx := tx.(Tx)
		for inputKey := range tx.InputIDs() {
			spenders := c.utxos[inputKey]
			spenders.Remove(txID)
			if spenders.Len() == 0 {
				delete(c.utxos, inputKey)
			} else {
				c.utxos[inputKey] = spenders
			}
		}

		delete(c.txs, txID.Key())
		c.pendingAccept.Abandon(txID)
		c.pendingReject.Fulfill(txID)
	}

	return acceptable, rejectable
}