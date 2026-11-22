package main

import (
	"encoding/json"
	"errors"
)

const dreamTransferLedgerSchema = "dream_transfer_local_ledger.v1"

type dreamTransferLedger struct {
	SchemaID string                 `json:"schema_id"`
	Receipts []dreamTransferReceipt `json:"receipts"`
}

// Optional transfers have their own size bound and write lock. Ordinary Mesh
// reads, exports, imports and grants never read or rewrite this ledger.
func (s *contextMeshStore) dreamLedgerPath() string { return s.path + ".dream-transfers.v1.json" }

func (s *contextMeshStore) loadDreamTransferReceiptsLocked() error {
	path := s.dreamLedgerPath()
	stamp, err := memoryEdgeLogPlatformFileStamp(path)
	if err != nil {
		return err
	}
	if !stamp.Exists {
		if len(s.dreamTransferReceipts) != 0 {
			return errors.New("active transfer ledger disappeared")
		}
		return nil
	}
	if stamp == s.dreamTransferStamp && len(s.dreamTransferReceipts) != 0 {
		return nil
	}
	raw, err := readOwnerOnlyBoundedFile(path, int64(s.maxBytes))
	if err != nil {
		return err
	}
	var ledger dreamTransferLedger
	if strictJSONDecode(raw, &ledger) != nil || ledger.SchemaID != dreamTransferLedgerSchema || ledger.Receipts == nil ||
		len(ledger.Receipts) > s.maxReceipts || s.identity == nil {
		return errors.New("transfer ledger invalid or over limit")
	}
	if len(ledger.Receipts) < len(s.dreamTransferReceipts) {
		return errors.New("transfer ledger lost an observed receipt")
	}
	seen := make(map[string]bool, len(ledger.Receipts))
	for i, receipt := range ledger.Receipts {
		if i < len(s.dreamTransferReceipts) && (s.dreamTransferReceipts[i].ReceiptID != receipt.ReceiptID ||
			s.dreamTransferReceipts[i].Signature != receipt.Signature) {
			return errors.New("transfer ledger replaced an observed receipt")
		}
		if seen[receipt.ReceiptID] || !verifyDreamTransferReceipt(receipt) || receipt.Issuer.InstanceID != s.identity.InstanceID ||
			receipt.Issuer.SigningKeyID != s.identity.SigningKeyID || receipt.Issuer.SigningPublicKey != s.identity.SigningPublicKey {
			return errors.New("transfer ledger contains invalid authority")
		}
		seen[receipt.ReceiptID] = true
	}
	after, err := memoryEdgeLogPlatformFileStamp(path)
	if err != nil || after != stamp {
		return errors.New("transfer ledger changed during read")
	}
	s.dreamTransferReceipts, s.dreamTransferStamp = ledger.Receipts, stamp
	return nil
}

func (s *contextMeshStore) saveDreamTransferReceiptsLocked() error {
	raw, err := json.Marshal(dreamTransferLedger{SchemaID: dreamTransferLedgerSchema, Receipts: s.dreamTransferReceipts})
	if err != nil || len(raw)+1 > s.maxBytes || len(s.dreamTransferReceipts) > s.maxReceipts {
		return errors.New("transfer ledger capacity exceeded")
	}
	if err := writeOwnerOnlyDurableAtomicFile(s.dreamLedgerPath(), append(raw, '\n'), false); err != nil {
		return err
	}
	// Revalidate from the verified file on next access, including uncertain
	// fsync outcomes. Never bless a stamp obtained after an unobserved replacement.
	s.dreamTransferStamp = memoryEdgeLogFileStamp{}
	return nil
}
