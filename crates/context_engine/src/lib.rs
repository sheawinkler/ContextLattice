use std::collections::HashMap;

use anyhow::{anyhow, Result};
use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct MemoryNode {
    pub id: String,
    pub project: String,
    pub file_name: String,
    pub content: String,
    pub topic_path: Option<String>,
}

#[derive(Default)]
pub struct ContextEngine {
    nodes: HashMap<String, MemoryNode>,
}

impl ContextEngine {
    pub fn add_memory(&mut self, node: MemoryNode) -> String {
        let id = node.id.clone();
        self.nodes.insert(id.clone(), node);
        id
    }

    pub fn update_memory(&mut self, memory_id: &str, content: String) -> Result<()> {
        let entry = self
            .nodes
            .get_mut(memory_id)
            .ok_or_else(|| anyhow!("memory not found: {memory_id}"))?;
        entry.content = content;
        Ok(())
    }

    pub fn get_memory(&self, memory_id: &str) -> Option<MemoryNode> {
        self.nodes.get(memory_id).cloned()
    }

    pub fn query_neighbors(&self, memory_id: &str, limit: usize) -> Vec<MemoryNode> {
        let root = match self.nodes.get(memory_id) {
            Some(node) => node,
            None => return Vec::new(),
        };
        self
            .nodes
            .values()
            .filter(|node| {
                node.id != root.id
                    && (node.project == root.project
                        || (node.topic_path.is_some() && node.topic_path == root.topic_path))
            })
            .take(limit)
            .cloned()
            .collect()
    }

    pub fn batch_insert(&mut self, nodes: Vec<MemoryNode>) -> Vec<String> {
        nodes.into_iter().map(|node| self.add_memory(node)).collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn node(id: &str, project: &str, file_name: &str, topic_path: &str) -> MemoryNode {
        MemoryNode {
            id: id.to_string(),
            project: project.to_string(),
            file_name: file_name.to_string(),
            content: "payload".to_string(),
            topic_path: Some(topic_path.to_string()),
        }
    }

    #[test]
    fn query_neighbors_prefers_shared_project_or_topic() {
        let mut engine = ContextEngine::default();
        let root_id = engine.add_memory(node("a", "alpha", "a.md", "root/topic"));
        engine.add_memory(node("b", "alpha", "b.md", "root/topic"));
        engine.add_memory(node("c", "beta", "c.md", "other/topic"));

        let neighbors = engine.query_neighbors(&root_id, 10);
        assert_eq!(neighbors.len(), 1);
        assert_eq!(neighbors[0].id, "b");
    }
}
