use ahash::AHashMap;
use aho_corasick::AhoCorasick;
use fst::{automaton::Str, Automaton, IntoStreamer, Set, Streamer};
use regex_automata::meta::Regex;
use roaring::RoaringBitmap;
use serde::{Deserialize, Serialize};
use simsimd::SpatialSimilarity;
use std::collections::{BTreeSet, HashMap};
use std::sync::OnceLock;

const LEXICAL_TOKEN_PATTERN: &str = r"[A-Za-z0-9]{3,}";
const LEXICAL_PREFIX_MAX_EXPANSIONS_DEFAULT: usize = 24;
const LEXICAL_SCAN_THRESHOLD_DEFAULT: usize = 64;

fn lexical_regex() -> &'static Regex {
    static REGEX: OnceLock<Regex> = OnceLock::new();
    REGEX.get_or_init(|| Regex::new(LEXICAL_TOKEN_PATTERN).expect("valid lexical token regex"))
}

fn lexical_prefix_max_expansions() -> usize {
    static VALUE: OnceLock<usize> = OnceLock::new();
    *VALUE.get_or_init(|| {
        std::env::var("CONTEXT_RETRIEVAL_LEXICAL_PREFIX_MAX_EXPANSIONS")
            .ok()
            .and_then(|raw| raw.trim().parse::<usize>().ok())
            .filter(|value| *value > 0)
            .unwrap_or(LEXICAL_PREFIX_MAX_EXPANSIONS_DEFAULT)
    })
}

fn lexical_scan_threshold() -> usize {
    static VALUE: OnceLock<usize> = OnceLock::new();
    *VALUE.get_or_init(|| {
        std::env::var("CONTEXT_RETRIEVAL_LEXICAL_SCAN_THRESHOLD")
            .ok()
            .and_then(|raw| raw.trim().parse::<usize>().ok())
            .filter(|value| *value > 0)
            .unwrap_or(LEXICAL_SCAN_THRESHOLD_DEFAULT)
    })
}

fn lexical_tokens(input: &str) -> Vec<String> {
    let lower = input.to_ascii_lowercase();
    let mut out = Vec::new();
    for m in lexical_regex().find_iter(lower.as_bytes()) {
        let token = lower[m.start()..m.end()].trim();
        if token.is_empty() || matches!(token, "root" | "notes" | "tasks" | "task" | "tmp") {
            continue;
        }
        out.push(token.to_string());
    }
    out.sort();
    out.dedup();
    out
}

fn lexical_phrase_match_score(needle: &str, text: &str) -> f32 {
    let n = needle.trim();
    if n.is_empty() {
        return 0.0;
    }
    let mut pattern = String::with_capacity(n.len());
    pattern.push_str(n);
    let Ok(ac) = AhoCorasick::new([pattern.as_str()]) else {
        return 0.0;
    };
    let count = ac.find_iter(text).count();
    if count == 0 {
        return 0.0;
    }
    let len_boost = (n.len() as f32 / 64.0).min(1.0);
    (count as f32).min(4.0) * (0.15 + len_boost * 0.35)
}

fn simd_blend_score(vector_score: f32, lexical_score: f32) -> f32 {
    let signal = [vector_score.max(0.0), lexical_score.max(0.0)];
    let weights = [0.85f32, 1.0f32];
    if let Some(dot) = <f32 as SpatialSimilarity>::dot(&signal, &weights) {
        return dot as f32;
    }
    vector_score + lexical_score
}

#[derive(Default)]
struct LexicalPostingIndex {
    postings: AHashMap<String, RoaringBitmap>,
    vocab: Option<Set<Vec<u8>>>,
}

impl LexicalPostingIndex {
    fn from_rows(rows: &[LexicalCandidate]) -> Self {
        let mut postings: AHashMap<String, RoaringBitmap> = AHashMap::new();
        let mut terms: BTreeSet<String> = BTreeSet::new();
        for (idx, row) in rows.iter().enumerate() {
            let row_id = idx as u32;
            for token in lexical_tokens(&row.text) {
                terms.insert(token.clone());
                postings.entry(token).or_default().insert(row_id);
            }
        }
        let vocab = if terms.is_empty() {
            None
        } else {
            Set::from_iter(terms.iter().map(|term| term.as_str())).ok()
        };
        Self { postings, vocab }
    }

    fn lookup_prefix_union(&self, token: &str) -> RoaringBitmap {
        let mut out = RoaringBitmap::new();
        let Some(vocab) = self.vocab.as_ref() else {
            return out;
        };
        let mut expansions = 0usize;
        let automaton = Str::new(token).starts_with();
        let mut stream = vocab.search(automaton).into_stream();
        while let Some(term_bytes) = stream.next() {
            let Ok(term) = std::str::from_utf8(term_bytes) else {
                continue;
            };
            if let Some(bitmap) = self.postings.get(term) {
                out |= bitmap;
                expansions += 1;
                if expansions >= lexical_prefix_max_expansions() {
                    break;
                }
            }
        }
        out
    }

    fn candidate_rows(&self, query_tokens: &[String], row_count: usize) -> RoaringBitmap {
        if row_count == 0 {
            return RoaringBitmap::new();
        }
        if query_tokens.is_empty() {
            return RoaringBitmap::from_iter(0..row_count as u32);
        }
        let mut aggregate: Option<RoaringBitmap> = None;
        for token in query_tokens {
            let token_hits = if let Some(bitmap) = self.postings.get(token) {
                bitmap.clone()
            } else {
                self.lookup_prefix_union(token)
            };
            if token_hits.is_empty() {
                return RoaringBitmap::new();
            }
            aggregate = match aggregate.take() {
                Some(current) => Some(current & token_hits),
                None => Some(token_hits),
            };
        }
        aggregate.unwrap_or_default()
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct RetrievalCandidate {
    pub id: String,
    pub score: f32,
    pub summary: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct SourceRetrievalCandidate {
    pub id: String,
    pub score: f32,
    pub summary: String,
    pub source: String,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
pub struct FusedRetrievalCandidate {
    pub id: String,
    pub score: f32,
    pub summary: String,
    pub sources: Vec<String>,
}

pub fn fuse_source_candidates(
    candidates: &[SourceRetrievalCandidate],
    limit: usize,
    consensus_boost: f32,
) -> Vec<FusedRetrievalCandidate> {
    if limit == 0 || candidates.is_empty() {
        return Vec::new();
    }

    #[derive(Clone)]
    struct MergeState {
        best_score: f32,
        best_summary: String,
        source_bitmap: RoaringBitmap,
    }

    let mut source_ids: AHashMap<String, u32> = AHashMap::new();
    let mut source_rev: Vec<String> = Vec::new();
    let mut merged: HashMap<String, MergeState> = HashMap::new();
    for candidate in candidates {
        let id = candidate.id.trim();
        if id.is_empty() {
            continue;
        }
        let source = {
            let normalized = candidate.source.trim().to_lowercase();
            if normalized.is_empty() {
                "unknown".to_string()
            } else {
                normalized
            }
        };
        let source_id = if let Some(existing) = source_ids.get(&source).copied() {
            existing
        } else {
            let next = source_rev.len() as u32;
            source_ids.insert(source.clone(), next);
            source_rev.push(source);
            next
        };
        if let Some(existing) = merged.get_mut(id) {
            existing.source_bitmap.insert(source_id);
            if candidate.score > existing.best_score {
                existing.best_score = candidate.score;
                existing.best_summary = candidate.summary.clone();
            } else if existing.best_summary.trim().is_empty()
                && !candidate.summary.trim().is_empty()
            {
                existing.best_summary = candidate.summary.clone();
            }
            continue;
        }
        let mut source_bitmap = RoaringBitmap::new();
        source_bitmap.insert(source_id);
        merged.insert(
            id.to_string(),
            MergeState {
                best_score: candidate.score,
                best_summary: candidate.summary.clone(),
                source_bitmap,
            },
        );
    }

    let mut rows: Vec<FusedRetrievalCandidate> = merged
        .into_iter()
        .map(|(id, state)| {
            let mut sources: Vec<String> = state
                .source_bitmap
                .iter()
                .filter_map(|idx| source_rev.get(idx as usize).cloned())
                .collect();
            sources.sort();
            let consensus = usize::saturating_sub(state.source_bitmap.len() as usize, 1) as f32;
            FusedRetrievalCandidate {
                id,
                score: state.best_score + consensus * consensus_boost.max(0.0),
                summary: state.best_summary,
                sources,
            }
        })
        .collect();

    rows.sort_by(|a, b| {
        b.score
            .partial_cmp(&a.score)
            .unwrap_or(std::cmp::Ordering::Equal)
            .then_with(|| a.id.cmp(&b.id))
    });
    rows.into_iter().take(limit).collect()
}

#[derive(Default)]
pub struct RetrievalIndex {
    rows: Vec<RetrievalCandidate>,
}

impl RetrievalIndex {
    pub fn upsert(&mut self, candidate: RetrievalCandidate) {
        if let Some(idx) = self.rows.iter().position(|row| row.id == candidate.id) {
            self.rows[idx] = candidate;
            return;
        }
        self.rows.push(candidate);
    }

    pub fn search(&self, limit: usize) -> Vec<RetrievalCandidate> {
        let mut rows = self.rows.clone();
        rows.sort_by(|a, b| {
            b.score
                .partial_cmp(&a.score)
                .unwrap_or(std::cmp::Ordering::Equal)
        });
        rows.into_iter().take(limit).collect()
    }

    pub fn batch_search(&self, limits: &[usize]) -> Vec<Vec<RetrievalCandidate>> {
        limits.iter().map(|limit| self.search(*limit)).collect()
    }
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct RetrievalBackendCapabilities {
    pub qdrant_remote_enabled: bool,
    pub usearch_ann_enabled: bool,
    pub tantivy_lexical_enabled: bool,
}

pub fn backend_capabilities() -> RetrievalBackendCapabilities {
    RetrievalBackendCapabilities {
        qdrant_remote_enabled: cfg!(feature = "qdrant_remote"),
        usearch_ann_enabled: cfg!(feature = "usearch_ann"),
        tantivy_lexical_enabled: cfg!(feature = "tantivy_lexical"),
    }
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct HybridQuery {
    pub text_query: Option<String>,
    pub vector_query: Option<Vec<f32>>,
    pub limit: usize,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct LexicalCandidate {
    pub id: String,
    pub text: String,
    pub score: f32,
}

#[derive(Default)]
pub struct HybridRetrievalIndex {
    vector: RetrievalIndex,
    lexical: Vec<LexicalCandidate>,
}

impl HybridRetrievalIndex {
    pub fn upsert_vector(&mut self, candidate: RetrievalCandidate) {
        self.vector.upsert(candidate);
    }

    pub fn upsert_lexical(&mut self, candidate: LexicalCandidate) {
        if let Some(idx) = self.lexical.iter().position(|row| row.id == candidate.id) {
            self.lexical[idx] = candidate;
            return;
        }
        self.lexical.push(candidate);
    }

    pub fn search(&self, query: &HybridQuery) -> Vec<RetrievalCandidate> {
        let limit = usize::max(1, query.limit);
        let mut merged = self.vector.search(limit * 2);
        if let Some(text_query) = query.text_query.as_ref() {
            let needle = text_query.trim().to_lowercase();
            if !needle.is_empty() {
                let index = LexicalPostingIndex::from_rows(&self.lexical);
                let tokens = lexical_tokens(&needle);
                let candidate_bitmap = if self.lexical.len() >= lexical_scan_threshold() {
                    index.candidate_rows(&tokens, self.lexical.len())
                } else {
                    RoaringBitmap::from_iter(0..self.lexical.len() as u32)
                };
                let iter: Box<dyn Iterator<Item = u32>> = if candidate_bitmap.is_empty() {
                    Box::new(0..self.lexical.len() as u32)
                } else {
                    Box::new(candidate_bitmap.iter())
                };
                for row_idx in iter {
                    let Some(row) = self.lexical.get(row_idx as usize) else {
                        continue;
                    };
                    let row_lc = row.text.to_lowercase();
                    let phrase_boost = lexical_phrase_match_score(&needle, &row_lc);
                    let mut matched = phrase_boost > 0.0;
                    if !matched && !tokens.is_empty() {
                        matched = tokens.iter().all(|t| row_lc.contains(t));
                    }
                    if matched {
                        merged.push(RetrievalCandidate {
                            id: row.id.clone(),
                            score: simd_blend_score(row.score, phrase_boost),
                            summary: row.text.clone(),
                        });
                    }
                }
            }
        }
        merged.sort_by(|a, b| {
            b.score
                .partial_cmp(&a.score)
                .unwrap_or(std::cmp::Ordering::Equal)
        });
        merged.into_iter().take(limit).collect()
    }
}

#[cfg(feature = "qdrant_remote")]
pub mod qdrant_remote {
    use anyhow::Result;
    use qdrant_client::Qdrant;

    pub struct QdrantRemoteAdapter {
        endpoint: String,
        api_key: Option<String>,
    }

    impl QdrantRemoteAdapter {
        pub fn new(endpoint: impl Into<String>, api_key: Option<String>) -> Self {
            Self {
                endpoint: endpoint.into(),
                api_key,
            }
        }

        pub fn endpoint(&self) -> &str {
            &self.endpoint
        }

        pub fn has_api_key(&self) -> bool {
            self.api_key
                .as_ref()
                .is_some_and(|value| !value.trim().is_empty())
        }

        pub fn build_client(&self) -> Result<Qdrant> {
            let mut builder = Qdrant::from_url(&self.endpoint);
            if let Some(api_key) = self.api_key.as_ref() {
                if !api_key.trim().is_empty() {
                    builder = builder.api_key(api_key.clone());
                }
            }
            Ok(builder.build()?)
        }
    }
}

#[cfg(feature = "usearch_ann")]
pub mod usearch_ann {
    use std::collections::HashMap;

    use anyhow::{anyhow, Context, Result};
    use serde::{Deserialize, Serialize};
    use usearch::{Index, IndexOptions, Key, MetricKind, ScalarKind};

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
    pub struct UsearchMatch {
        pub id: String,
        pub distance: f32,
    }

    pub struct UsearchAnnAdapter {
        index: Index,
        dimensions: usize,
        next_key: Key,
        key_by_id: HashMap<String, Key>,
        id_by_key: HashMap<Key, String>,
    }

    impl UsearchAnnAdapter {
        pub fn new(dimensions: usize) -> Result<Self> {
            if dimensions == 0 {
                return Err(anyhow!("dimensions must be > 0"));
            }
            let mut options = IndexOptions::default();
            options.dimensions = dimensions;
            options.metric = MetricKind::Cos;
            options.quantization = ScalarKind::F32;
            options.multi = false;
            let index = Index::new(&options).context("create usearch index")?;
            Ok(Self {
                index,
                dimensions,
                next_key: 1,
                key_by_id: HashMap::new(),
                id_by_key: HashMap::new(),
            })
        }

        pub fn backend_name(&self) -> &'static str {
            "usearch"
        }

        pub fn dimensions(&self) -> usize {
            self.dimensions
        }

        pub fn len(&self) -> usize {
            self.index.size()
        }

        pub fn is_empty(&self) -> bool {
            self.len() == 0
        }

        pub fn reserve(&self, capacity: usize) -> Result<()> {
            self.index
                .reserve(capacity)
                .context("reserve usearch capacity")
        }

        pub fn upsert(&mut self, id: impl Into<String>, vector: &[f32]) -> Result<()> {
            if vector.len() != self.dimensions {
                return Err(anyhow!(
                    "vector dimensions mismatch: expected {}, got {}",
                    self.dimensions,
                    vector.len()
                ));
            }
            let id = id.into();
            let key = if let Some(existing) = self.key_by_id.get(&id).copied() {
                let _ = self.index.remove(existing);
                existing
            } else {
                let next = self.next_key;
                self.next_key = self.next_key.saturating_add(1);
                self.key_by_id.insert(id.clone(), next);
                self.id_by_key.insert(next, id.clone());
                next
            };
            self.index
                .add(key, vector)
                .with_context(|| format!("insert vector for id {}", id))
        }

        pub fn remove(&mut self, id: &str) -> Result<bool> {
            let Some(key) = self.key_by_id.remove(id) else {
                return Ok(false);
            };
            self.id_by_key.remove(&key);
            let removed = self
                .index
                .remove(key)
                .with_context(|| format!("remove vector for id {}", id))?;
            Ok(removed > 0)
        }

        pub fn clear(&mut self) {
            self.key_by_id.clear();
            self.id_by_key.clear();
            self.next_key = 1;
            if let Ok(rebuilt) = Self::new(self.dimensions) {
                self.index = rebuilt.index;
            }
        }

        pub fn query(&self, vector: &[f32], limit: usize) -> Result<Vec<UsearchMatch>> {
            if vector.len() != self.dimensions {
                return Err(anyhow!(
                    "query dimensions mismatch: expected {}, got {}",
                    self.dimensions,
                    vector.len()
                ));
            }
            if limit == 0 {
                return Ok(Vec::new());
            }
            let matches = self
                .index
                .search(vector, limit)
                .context("usearch query failed")?;
            let mut out = Vec::with_capacity(matches.keys.len());
            for (idx, key) in matches.keys.iter().enumerate() {
                let Some(id) = self.id_by_key.get(key) else {
                    continue;
                };
                let distance = *matches.distances.get(idx).unwrap_or(&f32::MAX);
                out.push(UsearchMatch {
                    id: id.clone(),
                    distance,
                });
            }
            Ok(out)
        }
    }

    impl Default for UsearchAnnAdapter {
        fn default() -> Self {
            // This path should only fail for invalid static configuration.
            Self::new(384).expect("usearch default dimensions should construct")
        }
    }
}

#[cfg(feature = "tantivy_lexical")]
pub mod tantivy_lexical {
    #[allow(unused_imports)]
    use tantivy as _tantivy_dep;

    pub struct TantivyLexicalAdapter;

    impl Default for TantivyLexicalAdapter {
        fn default() -> Self {
            Self
        }
    }

    impl TantivyLexicalAdapter {
        pub fn backend_name(&self) -> &'static str {
            "tantivy"
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn search_returns_best_scores_first() {
        let mut index = RetrievalIndex::default();
        index.upsert(RetrievalCandidate {
            id: "a".to_string(),
            score: 0.1,
            summary: "low".to_string(),
        });
        index.upsert(RetrievalCandidate {
            id: "b".to_string(),
            score: 0.9,
            summary: "high".to_string(),
        });

        let rows = index.search(1);
        assert_eq!(rows.len(), 1);
        assert_eq!(rows[0].id, "b");
    }

    #[test]
    fn hybrid_search_merges_vector_and_lexical_results() {
        let mut index = HybridRetrievalIndex::default();
        index.upsert_vector(RetrievalCandidate {
            id: "vec-1".to_string(),
            score: 0.8,
            summary: "vector result".to_string(),
        });
        index.upsert_lexical(LexicalCandidate {
            id: "lex-1".to_string(),
            text: "profitability tuning baseline".to_string(),
            score: 0.7,
        });
        let rows = index.search(&HybridQuery {
            text_query: Some("baseline".to_string()),
            vector_query: None,
            limit: 5,
        });
        assert_eq!(rows.len(), 2);
        assert_eq!(rows[0].id, "vec-1");
        assert_eq!(rows[1].id, "lex-1");
    }

    #[test]
    fn backend_capabilities_reflect_feature_flags() {
        let caps = backend_capabilities();
        assert_eq!(caps.qdrant_remote_enabled, cfg!(feature = "qdrant_remote"));
        assert_eq!(caps.usearch_ann_enabled, cfg!(feature = "usearch_ann"));
        assert_eq!(
            caps.tantivy_lexical_enabled,
            cfg!(feature = "tantivy_lexical")
        );
    }

    #[test]
    fn fuse_source_candidates_merges_duplicate_ids_and_sources() {
        let rows = vec![
            SourceRetrievalCandidate {
                id: "alpha::a.md".to_string(),
                score: 0.81,
                summary: "from qdrant".to_string(),
                source: "qdrant".to_string(),
            },
            SourceRetrievalCandidate {
                id: "alpha::a.md".to_string(),
                score: 0.92,
                summary: "from topic rollups".to_string(),
                source: "topic_rollups".to_string(),
            },
            SourceRetrievalCandidate {
                id: "alpha::b.md".to_string(),
                score: 0.75,
                summary: "from mindsdb".to_string(),
                source: "mindsdb".to_string(),
            },
        ];
        let fused = fuse_source_candidates(&rows, 10, 0.0);
        assert_eq!(fused.len(), 2);
        assert_eq!(fused[0].id, "alpha::a.md");
        assert_eq!(fused[0].score, 0.92);
        assert_eq!(
            fused[0].sources,
            vec!["qdrant".to_string(), "topic_rollups".to_string()]
        );
        assert_eq!(fused[1].id, "alpha::b.md");
    }

    #[test]
    fn fuse_source_candidates_applies_consensus_boost() {
        let rows = vec![
            SourceRetrievalCandidate {
                id: "alpha::a.md".to_string(),
                score: 0.50,
                summary: "first".to_string(),
                source: "qdrant".to_string(),
            },
            SourceRetrievalCandidate {
                id: "alpha::a.md".to_string(),
                score: 0.50,
                summary: "second".to_string(),
                source: "topic_rollups".to_string(),
            },
        ];
        let fused = fuse_source_candidates(&rows, 5, 0.10);
        assert_eq!(fused.len(), 1);
        assert!((fused[0].score - 0.60).abs() < 0.0001);
        assert_eq!(fused[0].sources.len(), 2);
    }

    #[cfg(feature = "usearch_ann")]
    #[test]
    #[ignore = "native usearch runtime can segfault on some hosts; compile coverage enforced via cargo check --features usearch_ann"]
    fn usearch_ann_upsert_query_remove_roundtrip() {
        let mut ann = usearch_ann::UsearchAnnAdapter::new(3).expect("build usearch");
        ann.upsert("doc-a", &[0.9, 0.1, 0.0]).expect("insert a");
        ann.upsert("doc-b", &[0.1, 0.9, 0.0]).expect("insert b");
        ann.upsert("doc-c", &[0.1, 0.0, 0.9]).expect("insert c");
        let rows = ann.query(&[0.95, 0.05, 0.0], 2).expect("query");
        assert!(!rows.is_empty());
        assert_eq!(rows[0].id, "doc-a");
        assert!(ann.remove("doc-a").expect("remove"));
        let rows_after = ann.query(&[0.95, 0.05, 0.0], 2).expect("query 2");
        assert!(rows_after.iter().all(|row| row.id != "doc-a"));
    }

    #[cfg(feature = "usearch_ann")]
    #[test]
    #[ignore = "native usearch runtime can segfault on some hosts; compile coverage enforced via cargo check --features usearch_ann"]
    fn usearch_ann_rejects_dimension_mismatch() {
        let mut ann = usearch_ann::UsearchAnnAdapter::new(4).expect("build usearch");
        assert!(ann.upsert("bad", &[0.1, 0.2]).is_err());
        assert!(ann.query(&[0.1, 0.2], 1).is_err());
    }
}
