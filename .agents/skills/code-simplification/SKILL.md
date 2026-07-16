---
name: code-simplification
description: Simplifies code for clarity
---

# Code Simplification

> Inspired by the [Claude Code Simplifier plugin](https://github.com/anthropics/claude-plugins-official/blob/main/plugins/code-simplifier/agents/code-simplifier.md). Adapted here as a model-agnostic, process-driven skill for any AI coding agent.

## Overview

Simplify code by reducing complexity while preserving exact behavior. The goal is not fewer lines — it's code that is easier to read, understand, modify, and debug. Every simplification must pass a simple test: "Would a new team member understand this faster than the original?"

## When to Use

- After a feature is working and tests pass, but the implementation feels heavier than it needs to be
- During code review when readability or complexity issues are flagged
- When you encounter deeply nested logic, long functions, or unclear names
- When refactoring code written under time pressure
- When consolidating related logic scattered across files
- After merging changes that introduced duplication or inconsistency

**When NOT to use:**

- Code is already clean and readable — don't simplify for the sake of it
- You don't understand what the code does yet — comprehend before you simplify
- The code is performance-critical and the "simpler" version would be measurably slower
- You're about to rewrite the module entirely — simplifying throwaway code wastes effort

## The Five Principles

### 1. Preserve Behavior Exactly

Don't change what the code does — only how it expresses it. All inputs, outputs, side effects, error behavior, and edge cases must remain identical.

### 2. Follow Project Conventions

Simplification means making code more consistent with the codebase, not imposing external preferences.

### 3. Prefer Clarity Over Cleverness

Explicit code is better than compact code when the compact version requires a mental pause to parse.

### 4. Maintain Balance

Simplification has a failure mode: over-simplification. Watch for these traps:

- Inlining too aggressively
- Combining unrelated logic
- Removing "unnecessary" abstraction
- Optimizing for line count

### 5. Scope to What Changed

Default to simplifying recently modified code. Avoid drive-by refactors of unrelated code.

## The Simplification Process

### Step 1: Understand Before Touching (Chesterton's Fence)

Before changing or removing anything, understand why it exists.

### Step 2: Identify Simplification Opportunities

Scan for structural complexity, naming/readability issues, and redundancy.

### Step 3: Apply Changes Incrementally

Make one simplification at a time. Run tests after each change.

### Step 4: Verify the Result

After all simplifications, step back and evaluate the whole.

## Red Flags

- Simplification that requires modifying tests to pass
- "Simplified" code that is longer and harder to follow than the original
- Renaming things to match your preferences rather than project conventions
- Removing error handling because "it makes the code cleaner"
- Simplifying code you don't fully understand
- Batching many simplifications into one large, hard-to-review commit
