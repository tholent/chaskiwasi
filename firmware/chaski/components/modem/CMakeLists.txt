# Target-only component: wraps the vendor walter-modem library. Deliberately
# absent from the host build — there is nothing to test off-hardware here, and
# every decision worth testing lives above the transport seam.
idf_component_register(SRCS "modem.cpp"
                       INCLUDE_DIRS "include"
                       REQUIRES transport)
