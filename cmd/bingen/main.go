package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/shabbyrobe/bingen"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	bmc := bingen.Command{}

	var fs = &flag.FlagSet{}
	bmc.Flags(fs)
	if err := fs.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			help(&bmc, fs)
			return nil
		}
		return err
	}

	err := bmc.Run(fs.Args()...)
	if bingen.IsUsageError(err) {
		help(&bmc, fs)
		fmt.Println()
		fmt.Println("error:", err)
		return nil
	}
	return err
}

func help(bmc *bingen.Command, fs *flag.FlagSet) {
	fmt.Println(bmc.Usage())
	fmt.Println("Flags:")
	fs.PrintDefaults()
}
