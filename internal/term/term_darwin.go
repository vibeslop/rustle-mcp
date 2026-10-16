package term

import "syscall"

const getTermios, setTermios = syscall.TIOCGETA, syscall.TIOCSETA
