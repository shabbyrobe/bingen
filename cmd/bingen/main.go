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

	var help bool
	var fs = &flag.FlagSet{}
	bmc.Flags(fs)
	fs.BoolVar(&help, "help", false, "help")
	fs.Parse(os.Args[1:])
	if help {
		fmt.Println(bmc.Usage())
		return nil
	}

	err := bmc.Run(fs.Args()...)
	if bingen.IsUsageError(err) {
		fmt.Println(err)
		fmt.Println()
		fmt.Println(bmc.Usage())
		fmt.Println("Flags:")
		fs.PrintDefaults()
		return nil
	}
	return err
}
