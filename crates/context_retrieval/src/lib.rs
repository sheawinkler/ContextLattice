use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct RetrievalCandidate {
    pub id: String,
    pub score: f32,
    pub summary: String,
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
        rows.sort_by(|a, b| b.score.partial_cmp(&a.score).unwrap_or(std::cmp::Ordering::Equal));
        rows.into_iter().take(limit).collect()
    }

    pub fn batch_search(&self, limits: &[usize]) -> Vec<Vec<RetrievalCandidate>> {
        limits.iter().map(|limit| self.search(*limit)).collect()
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
}
