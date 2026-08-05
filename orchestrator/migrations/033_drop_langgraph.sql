-- Drops the dead LangGraph checkpoint schema created by 001_initial.sql and
-- 002_langgraph_checkpointer.sql. Nothing in the codebase has read or written
-- these tables since #247 replaced the Python/LangGraph orchestrator with the
-- Go state machine (see the comments on the two migrations above for
-- history); a fresh deployment gains nothing by still applying them.
DROP TABLE IF EXISTS langgraph.checkpoint_writes;
DROP TABLE IF EXISTS langgraph.checkpoints;
DROP SCHEMA IF EXISTS langgraph;
