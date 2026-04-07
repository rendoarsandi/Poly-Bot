import re

def remove_function(content, func_name):
    # Matches 'func func_name(...) {' and all its lines until a closing brace at column 0.
    pattern = r'^func ' + func_name + r'\([^)]*\)(?:.*?)? \{[\s\S]*?\n\}\n'
    return re.sub(pattern, '', content, flags=re.MULTILINE)

# REALBOT
with open("cmd/realbot/main.go", "r") as f:
    content = f.read()

content = remove_function(content, "ladderedTakerBiasedBuyShares")
content = remove_function(content, "ladderedTakerEntryMovedEnough")

with open("cmd/realbot/main.go", "w") as f:
    f.write(content)

with open("cmd/realbot/main_test.go", "r") as f:
    content = f.read()

content = remove_function(content, "TestLadderedTakerBiasedBuySharesOverweightsHigherAskOutcome")
content = remove_function(content, "TestLadderedTakerEntryMovedEnoughUsesMoveThreshold")

with open("cmd/realbot/main_test.go", "w") as f:
    f.write(content)

# PAPERBOT
with open("cmd/paperbot/main.go", "r") as f:
    content = f.read()

content = remove_function(content, "ladderedTakerBiasedBuyShares")
content = remove_function(content, "paperbotLadderedEntryMovedEnough")

with open("cmd/paperbot/main.go", "w") as f:
    f.write(content)

with open("cmd/paperbot/main_test.go", "r") as f:
    content = f.read()

content = remove_function(content, "TestPaperbotLadderedBiasedBuySharesOverweightsHigherAskOutcome")
content = remove_function(content, "TestPaperbotLadderedEntryMovedEnoughUsesMoveThreshold")

with open("cmd/paperbot/main_test.go", "w") as f:
    f.write(content)
