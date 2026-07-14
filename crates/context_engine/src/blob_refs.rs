use std::collections::{HashMap, HashSet};
use std::fs;
use std::io::{Read, Write};
use std::sync::OnceLock;
use std::time::{SystemTime, UNIX_EPOCH};

use anyhow::{anyhow, Result};
use lz4_flex::frame::{FrameDecoder, FrameEncoder};
use serde::{Deserialize, Serialize};

pub const BLOB_SCHEMA_VERSION: u16 = 1;
const BLOB_LZ4_MAX_BYTES_DEFAULT: usize = 8 * 1024;
const BLOB_ZSTD_LEVEL_DEFAULT: i32 = 3;

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub struct BlobRef {
    pub hash: String,
    pub schema_version: u16,
    pub codec: String,
    pub content_bytes: usize,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct BlobRecord {
    pub hash: String,
    pub schema_version: u16,
    pub codec: String,
    pub content_bytes: usize,
    pub payload: Vec<u8>,
    pub ref_count: u64,
    pub created_at_ms: u64,
    pub updated_at_ms: u64,
}

#[derive(Clone, Debug, Serialize, Deserialize, Default)]
pub struct BlobStoreMetrics {
    pub blobs: usize,
    pub refs: u64,
    pub bytes_raw: u64,
    pub bytes_stored: u64,
}

#[derive(Clone, Debug)]
pub struct BlobStore {
    records: HashMap<String, BlobRecord>,
    pub min_compress_bytes: usize,
    pub gc_grace_ms: u64,
}

impl Default for BlobStore {
    fn default() -> Self {
        Self::new(256, 24 * 60 * 60 * 1000)
    }
}

impl BlobStore {
    pub fn new(min_compress_bytes: usize, gc_grace_ms: u64) -> Self {
        Self {
            records: HashMap::new(),
            min_compress_bytes: min_compress_bytes.max(64),
            gc_grace_ms,
        }
    }

    pub fn put(&mut self, content: &[u8]) -> Result<BlobRef> {
        let hash = blake3::hash(content).to_hex().to_string();
        let now = now_ms();
        if let Some(existing) = self.records.get_mut(&hash) {
            existing.ref_count = existing.ref_count.saturating_add(1);
            existing.updated_at_ms = now;
            return Ok(BlobRef {
                hash,
                schema_version: existing.schema_version,
                codec: existing.codec.clone(),
                content_bytes: existing.content_bytes,
            });
        }

        let (codec, payload) = select_blob_codec_and_payload(content, self.min_compress_bytes)?;

        let record = BlobRecord {
            hash: hash.clone(),
            schema_version: BLOB_SCHEMA_VERSION,
            codec: codec.clone(),
            content_bytes: content.len(),
            payload,
            ref_count: 1,
            created_at_ms: now,
            updated_at_ms: now,
        };
        self.records.insert(hash.clone(), record);
        Ok(BlobRef {
            hash,
            schema_version: BLOB_SCHEMA_VERSION,
            codec,
            content_bytes: content.len(),
        })
    }

    pub fn materialize(&self, reference: &BlobRef) -> Result<Vec<u8>> {
        if reference.schema_version != BLOB_SCHEMA_VERSION {
            return Err(anyhow!(
                "unsupported blob schema version: {}",
                reference.schema_version
            ));
        }
        let record = self
            .records
            .get(&reference.hash)
            .ok_or_else(|| anyhow!("blob not found: {}", reference.hash))?;
        if record.schema_version != reference.schema_version {
            return Err(anyhow!(
                "schema mismatch for {}: store={} ref={}",
                reference.hash,
                record.schema_version,
                reference.schema_version
            ));
        }
        let decoded = match record.codec.as_str() {
            "raw" => record.payload.clone(),
            "lz4" => {
                let mut decoder = FrameDecoder::new(record.payload.as_slice());
                let mut out = Vec::with_capacity(record.content_bytes);
                decoder
                    .read_to_end(&mut out)
                    .map_err(|err| anyhow!("lz4 decode failed for {}: {err}", reference.hash))?;
                out
            }
            "zstd" => zstd::stream::decode_all(record.payload.as_slice())
                .map_err(|err| anyhow!("zstd decode failed for {}: {err}", reference.hash))?,
            "zstd_dict" => {
                let dict = zstd_dict_bytes().ok_or_else(|| {
                    anyhow!(
                        "zstd dict codec set for {} but CONTEXT_ENGINE_BLOB_ZSTD_DICT_PATH is unavailable",
                        reference.hash
                    )
                })?;
                let mut decoder =
                    zstd::stream::Decoder::with_dictionary(record.payload.as_slice(), dict)
                        .map_err(|err| {
                            anyhow!(
                                "zstd dict decoder init failed for {}: {err}",
                                reference.hash
                            )
                        })?;
                let mut out = Vec::with_capacity(record.content_bytes);
                decoder.read_to_end(&mut out).map_err(|err| {
                    anyhow!("zstd dict decode failed for {}: {err}", reference.hash)
                })?;
                out
            }
            other => {
                return Err(anyhow!(
                    "unsupported blob codec for {}: {}",
                    reference.hash,
                    other
                ))
            }
        };
        if decoded.len() != record.content_bytes {
            return Err(anyhow!(
                "decoded length mismatch for {}: got {} expected {}",
                reference.hash,
                decoded.len(),
                record.content_bytes
            ));
        }
        Ok(decoded)
    }

    pub fn release(&mut self, hash: &str) -> bool {
        let key = hash.trim();
        if key.is_empty() {
            return false;
        }
        let now = now_ms();
        if let Some(record) = self.records.get_mut(key) {
            record.ref_count = record.ref_count.saturating_sub(1);
            record.updated_at_ms = now;
            return true;
        }
        false
    }

    pub fn compact_orphans(&mut self, live_refs: &[String], now_ms: u64) -> Vec<String> {
        let live: HashSet<&str> = live_refs
            .iter()
            .map(|value| value.trim())
            .filter(|value| !value.is_empty())
            .collect();
        let mut removed = Vec::new();
        self.records.retain(|hash, record| {
            let orphan_candidate = record.ref_count == 0 && !live.contains(hash.as_str());
            if !orphan_candidate {
                return true;
            }
            let age_ms = now_ms.saturating_sub(record.updated_at_ms);
            if age_ms < self.gc_grace_ms {
                return true;
            }
            removed.push(hash.clone());
            false
        });
        removed
    }

    pub fn metrics(&self) -> BlobStoreMetrics {
        let mut refs: u64 = 0;
        let mut raw: u64 = 0;
        let mut stored: u64 = 0;
        for record in self.records.values() {
            refs = refs.saturating_add(record.ref_count);
            raw = raw.saturating_add(record.content_bytes as u64);
            stored = stored.saturating_add(record.payload.len() as u64);
        }
        BlobStoreMetrics {
            blobs: self.records.len(),
            refs,
            bytes_raw: raw,
            bytes_stored: stored,
        }
    }

    pub fn get_record(&self, hash: &str) -> Option<&BlobRecord> {
        self.records.get(hash)
    }
}

fn now_ms() -> u64 {
    let now = SystemTime::now();
    let duration = now.duration_since(UNIX_EPOCH).unwrap_or_default();
    duration.as_millis() as u64
}

fn blob_lz4_max_bytes() -> usize {
    static VALUE: OnceLock<usize> = OnceLock::new();
    *VALUE.get_or_init(|| {
        std::env::var("CONTEXT_ENGINE_BLOB_LZ4_MAX_BYTES")
            .ok()
            .and_then(|raw| raw.trim().parse::<usize>().ok())
            .filter(|value| *value > 0)
            .unwrap_or(BLOB_LZ4_MAX_BYTES_DEFAULT)
    })
}

fn blob_zstd_level() -> i32 {
    static VALUE: OnceLock<i32> = OnceLock::new();
    *VALUE.get_or_init(|| {
        std::env::var("CONTEXT_ENGINE_BLOB_ZSTD_LEVEL")
            .ok()
            .and_then(|raw| raw.trim().parse::<i32>().ok())
            .map(|value| value.clamp(1, 19))
            .unwrap_or(BLOB_ZSTD_LEVEL_DEFAULT)
    })
}

fn zstd_dict_bytes() -> Option<&'static [u8]> {
    static DICT: OnceLock<Option<Vec<u8>>> = OnceLock::new();
    DICT.get_or_init(|| {
        let path = std::env::var("CONTEXT_ENGINE_BLOB_ZSTD_DICT_PATH").ok()?;
        let trimmed = path.trim();
        if trimmed.is_empty() {
            return None;
        }
        fs::read(trimmed).ok()
    })
    .as_deref()
}

fn select_blob_codec_and_payload(
    content: &[u8],
    min_compress_bytes: usize,
) -> Result<(String, Vec<u8>)> {
    if content.len() < min_compress_bytes {
        return Ok(("raw".to_string(), content.to_vec()));
    }
    if content.len() <= blob_lz4_max_bytes() {
        let mut encoder = FrameEncoder::new(Vec::new());
        encoder
            .write_all(content)
            .map_err(|err| anyhow!("lz4 encode failed: {err}"))?;
        let payload = encoder
            .finish()
            .map_err(|err| anyhow!("lz4 finalize failed: {err}"))?;
        return Ok(("lz4".to_string(), payload));
    }
    if let Some(dict) = zstd_dict_bytes() {
        let mut encoder =
            zstd::stream::Encoder::with_dictionary(Vec::new(), blob_zstd_level(), dict)
                .map_err(|err| anyhow!("zstd dict encoder init failed: {err}"))?;
        encoder
            .write_all(content)
            .map_err(|err| anyhow!("zstd dict encode failed: {err}"))?;
        let payload = encoder
            .finish()
            .map_err(|err| anyhow!("zstd dict finalize failed: {err}"))?;
        return Ok(("zstd_dict".to_string(), payload));
    }
    let payload = zstd::stream::encode_all(content, blob_zstd_level())
        .map_err(|err| anyhow!("zstd encode failed: {err}"))?;
    Ok(("zstd".to_string(), payload))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn dedup_refs_reuse_single_blob() {
        let mut store = BlobStore::new(64, 1_000);
        let content = b"telemetry payload repeated";
        let first = store.put(content).expect("put first");
        let second = store.put(content).expect("put second");
        assert_eq!(first.hash, second.hash);
        let record = store.get_record(&first.hash).expect("record exists");
        assert_eq!(record.ref_count, 2);
        assert_eq!(store.metrics().blobs, 1);
    }

    #[test]
    fn roundtrip_materialize_preserves_payload() {
        let mut store = BlobStore::new(32, 1_000);
        let payload = vec![b'x'; 32 * 1024];
        let reference = store.put(&payload).expect("put");
        assert_eq!(reference.codec, "zstd");
        let decoded = store.materialize(&reference).expect("decode");
        assert_eq!(decoded, payload);
    }

    #[test]
    fn hot_payload_uses_lz4_codec() {
        let mut store = BlobStore::new(64, 1_000);
        let payload = vec![b'y'; 256];
        let reference = store.put(&payload).expect("put");
        assert_eq!(reference.codec, "lz4");
        let decoded = store.materialize(&reference).expect("decode");
        assert_eq!(decoded, payload);
    }

    #[test]
    fn gc_removes_orphans_after_grace_period() {
        let mut store = BlobStore::new(64, 100);
        let reference = store.put(b"orphan me").expect("put");
        assert!(store.release(&reference.hash));
        let now = now_ms() + 250;
        let removed = store.compact_orphans(&[], now);
        assert_eq!(removed.len(), 1);
        assert_eq!(removed[0], reference.hash);
        assert_eq!(store.metrics().blobs, 0);
    }

    #[test]
    fn gc_keeps_live_references() {
        let mut store = BlobStore::new(64, 100);
        let reference = store.put(b"still live").expect("put");
        assert!(store.release(&reference.hash));
        let now = now_ms() + 250;
        let removed = store.compact_orphans(std::slice::from_ref(&reference.hash), now);
        assert!(removed.is_empty());
        assert_eq!(store.metrics().blobs, 1);
    }
}
