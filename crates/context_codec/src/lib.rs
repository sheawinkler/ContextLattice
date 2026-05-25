use anyhow::Result;
use blake3::Hasher;
use serde::Serialize;
use serde_json::Value;

pub mod agent_contracts_generated;

pub fn encode_state<T: Serialize>(value: &T) -> Result<Vec<u8>> {
    Ok(serde_json::to_vec(value)?)
}

pub fn decode_state(payload: &[u8]) -> Result<Value> {
    Ok(serde_json::from_slice(payload)?)
}

pub fn encode_batch<T: Serialize>(items: &[T]) -> Result<Vec<u8>> {
    Ok(serde_json::to_vec(items)?)
}

pub fn decode_batch(payload: &[u8]) -> Result<Vec<Value>> {
    let value: Value = serde_json::from_slice(payload)?;
    match value {
        Value::Array(rows) => Ok(rows),
        other => Ok(vec![other]),
    }
}

pub fn checksum(payload: &[u8]) -> String {
    let mut hasher = Hasher::new();
    hasher.update(payload);
    hasher.finalize().to_hex().to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn round_trip_state() {
        let payload = json!({"foo": "bar", "count": 3});
        let encoded = encode_state(&payload).expect("encode");
        let decoded = decode_state(&encoded).expect("decode");
        assert_eq!(decoded["foo"], "bar");
        assert_eq!(decoded["count"], 3);
        assert!(!checksum(&encoded).is_empty());
    }
}
