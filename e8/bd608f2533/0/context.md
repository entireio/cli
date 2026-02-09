# Session Context

## User Prompts

### Prompt 1

can you check In CalculateAttributionWithAccumulated, TotalCommitted and AgentLines don't account for user removals of agent lines. When a user removes lines the agent added, totalCommitted is calculated as totalAgentAdded + pureUserAdded without subtracting pureUserRemoved. For example, if the agent adds 10 lines and the user removes 5 of them and adds 2 new ones, the commit has 7 net lines but TotalCommitted reports 10, and AgentPercentage reports 100% instead of ~71%. The simpler CalculateAtt...

### Prompt 2

can you fix it?

### Prompt 3

are there tests for CalculateAttributionWithAccumulated?

### Prompt 4

yes, all 3

