# This wrapper owns the inverted result; the expected failing child must
# not write a failing report to the wrapper's XML_OUTPUT_FILE.
unset XML_OUTPUT_FILE
! $*
