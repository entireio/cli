# Session Context

## User Prompts

### Prompt 1

Right now, to disable telemetry we use: Opt-out via ENTIRE_TELEMETRY_OPTOUT=1 environment variable

I would suggest we add this also to entire enable. We would like to gather some metrics, are you ok with that? something like that. I would be open about it and let people opt out. A bit like the Apple does.

### Prompt 2

All good, but, I would replace ENTIRE_TELEMETRY_OPTOUT with the preferences stored at settings files

### Prompt 3

its perfect

### Prompt 4

In terms of usability and wanting to have as many users with telemetry enabled. Which aproach you think is better?
1. Asking only once at entire enable, update the config and if telemetry is in the config, do not ask anymore.
2. something else?

### Prompt 5

let's implement options 2

### Prompt 6

run the test

### Prompt 7

if i run entire enable --no-telemetry, it displays options to select strategy and that stuff, but ends up not disabling telemetry. it only disables telemetry if i also specify --strategy .. , so it run fully in no interactive mode. why ?

### Prompt 8

I want to inform about telemetry only first time user calls entire enable.

### Prompt 9

it is still displaying it subsequents times

