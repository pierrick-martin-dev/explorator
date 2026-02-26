package main

const cppPrompt = `
Task: Analyze the following C/C++ component.
The codebase context (including these files) is loaded in your memory.

Rules for Extraction:
1. NAME: Extract the exact identifier (e.g., "CMainFrame"). Keep it strictly under 256 characters.
2. SYNTHESIS: Read the header (.h/.hpp) to identify the entities (classes, structs, functions). Read the implementation (.c/.cpp) to determine the legacy markers (e.g., uses_open_utm, uses_psdb) and dependencies.
3. CONCISENESS: Provide a structural summary. Do not output raw code blocks.

Files making up this component:
- %s
`

const makefilePrompt = `
Task: Analyze the following build script (Makefile).
The codebase context is loaded in your memory.

Rules for Extraction:
1. SUMMARY: Provide a 1-2 sentence explanation of what this Makefile compiles or links.
2. KEY ELEMENTS: List the primary build targets (e.g., "all", "clean", "libmgen.a").
3. REFERENCES: List any explicitly included sub-makefiles, scripts, or required binaries.

Files making up this component:
- %s
`

const xmlPrompt = `
Task: Analyze the following configuration or XML data file.
The codebase context is loaded in your memory.

Rules for Extraction:
1. SUMMARY: Provide a strictly factual explanation of what system, service, or feature this configures.
2. KEY ELEMENTS: List the root XML nodes, main configuration keys, or primary defined variables.
3. REFERENCES: List any external files, schemas, or network endpoints referenced inside.

Files making up this component:
- %s
`

const otherPrompt = `
Task: Analyze the following generic file.
The codebase context is loaded in your memory.

Rules for Extraction:
1. SUMMARY: Explain the general purpose of this file within the repository.
2. KEY ELEMENTS: Identify the most important structural parts, commands, or sections.
3. REFERENCES: List any other files or modules explicitly mentioned.

Files making up this component:
- %s
`

const rationalRosePrompt = `
Task: Analyze the following legacy Rational Rose UML model file (.cat or .mdl).
The codebase context is loaded in your memory.

Rules for Extraction:
1. SUMMARY: Provide a 1-2 sentence explanation of the telecom domain, subsystem, or feature this UML model defines.
2. KEY ELEMENTS: List the primary Management Objects (MOs), classes, or logical packages declared in this model.
3. REFERENCES: List any external UML packages, .cat files, or foundational models explicitly imported or used as base classes.

Files making up this component:
- %s
`

const shellPrompt = `
Task: Analyze the following shell script (e.g., .sh, .csh, .ksh, .bash).
The codebase context is loaded in your memory.

Rules for Extraction:
1. SUMMARY: Provide a strictly factual, 1-2 sentence explanation of what this script automates, sets up, or executes.
2. KEY ELEMENTS: List the primary environment variables it exports/requires (e.g., PROJECTHOME), main commands executed, or CLI arguments it accepts.
3. REFERENCES: List any explicitly sourced files, called scripts, or external binaries it depends on.

Files making up this component:
- %s
`
