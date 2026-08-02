# The screens. These link chaski_strings because the UI resolves text through
# the string-id enum, and the recording panel because what a screen renders is
# only assertable as a trace (see ../panel/recording_panel.h).
chaski_add_test(ui_test ui_test.cpp chaski_ui chaski_strings)
chaski_add_test(cover_test cover_test.cpp chaski_ui chaski_strings)
