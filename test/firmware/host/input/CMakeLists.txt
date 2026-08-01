# C-11's suite. Split by what it tests, not by file size: the dispatcher's
# interception is a device invariant, the KeebDeck source is one board's
# hardware, and the second is the part that will be rewritten when the real
# keyboard arrives.
chaski_add_test(input_test input_test.cpp chaski_input)
chaski_add_test(keebdeck_test keebdeck_test.cpp chaski_input)
