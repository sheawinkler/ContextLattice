use serde::{Deserialize, Serialize};
use std::collections::{HashMap, HashSet};

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
        sources: HashSet<String>,
    }

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
        if let Some(existing) = merged.get_mut(id) {
            existing.sources.insert(source);
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
        let mut sources = HashSet::new();
        sources.insert(source);
        merged.insert(
            id.to_string(),
            MergeState {
                best_score: candidate.score,
                best_summary: candidate.summary.clone(),
                sources,
            },
        );
    }

    let mut rows: Vec<FusedRetrievalCandidate> = merged
        .into_iter()
        .map(|(id, state)| {
            let mut sources: Vec<String> = state.sources.into_iter().collect();
            sources.sort();
            let consensus = usize::saturating_sub(sources.len(), 1) as f32;
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
                for row in &self.lexical {
                    if row.text.to_lowercase().contains(&needle) {
                        merged.push(RetrievalCandidate {
                            id: row.id.clone(),
                            score: row.score,
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
    #[allow(unused_imports)]
    use usearch as _usearch_dep;

    pub struct UsearchAnnAdapter;

    impl Default for UsearchAnnAdapter {
        fn default() -> Self {
            Self
        }
    }

    impl UsearchAnnAdapter {
        pub fn backend_name(&self) -> &'static str {
            "usearch"
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
}
