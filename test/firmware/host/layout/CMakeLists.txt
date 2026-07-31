# cjson: layout_test loads test/firmware/host/testdata/graphemes.json (C-9's
# server-generated vectors) rather than hand-transcribing them.
chaski_add_test(layout_test layout_test.cpp chaski_layout cjson)
