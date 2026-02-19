#!/usr/bin/env python3
import os
from generic_runner import main

if __name__ == "__main__":
    os.environ.setdefault("TASK_AGENT", "crewai")
    raise SystemExit(main(agent_label="crewai"))
